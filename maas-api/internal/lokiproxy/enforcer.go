// Package lokiproxy implements a read-only Loki proxy that restricts every
// query to the caller's organization.
package lokiproxy

import (
	"errors"
	"fmt"
	"net/url"
	"strings"

	logqlv2 "github.com/observatorium/api/logql/v2"
	"github.com/prometheus/prometheus/model/labels"
)

const queryParam = "query"

// maxQueryLength preserves the removed Python proxy's request bound and
// prevents unbounded LogQL parsing work before Loki applies its own limits.
const maxQueryLength = 32 * 1024

// ErrInvalidQuery is returned when the client supplied a query that cannot be
// parsed as a LogQL expression. The caller is expected to surface it as a 400.
var ErrInvalidQuery = errors.New("invalid LogQL query")

// enforceQueryMatchers rewrites the "query" parameter in v so that every log
// query is restricted by the required authorization matchers.
//
// The tenant matcher is injected into every stream selector because Loki
// stream selectors require indexed labels. Organization and user matchers are
// appended as post-parse label-filter stages on query endpoints, preserving
// compatibility with collectors that emit those fields as structured metadata
// rather than indexed stream labels. Label endpoints cannot enforce
// post-parse filters, so requests that require them are rejected.
func enforceQueryMatchers(v url.Values, required []*labels.Matcher, queryEndpoint bool) error {
	paramValue := v.Get(queryParam)
	if len(paramValue) > maxQueryLength {
		return fmt.Errorf("%w: query too long", ErrInvalidQuery)
	}

	selectorRequired, pipelineRequired := partitionRequiredMatchers(required)
	pipelineRequired = escapePipelineMatcherValues(pipelineRequired)
	if paramValue == "" {
		if queryEndpoint {
			return fmt.Errorf("%w: empty query", ErrInvalidQuery)
		}
		if len(pipelineRequired) > 0 {
			return fmt.Errorf("%w: label endpoints require indexed stream labels", ErrInvalidQuery)
		}
		if len(selectorRequired) == 0 {
			v.Del(queryParam)
			return nil
		}

		v.Set(queryParam, requiredSelector(selectorRequired))
		return nil
	}

	if queryEndpoint {
		expr, err := logqlv2.ParseExpr(paramValue)
		if err != nil {
			return fmt.Errorf("%w: %w", ErrInvalidQuery, err)
		}
		expr.Walk(func(e any) {
			switch le := e.(type) {
			case *logqlv2.LogQueryExpr:
				le.SetMatchers(combineLabelMatchers(le.Matchers(), selectorRequired))
				le.AppendPipelineMatchers(pipelineRequired, "and")
			case *logqlv2.StreamMatcherExpr:
				le.SetMatchers(combineLabelMatchers(le.Matchers(), selectorRequired))
			}
		})

		rewritten := expr.String()
		// Reparse the AST-rendered query. This also rejects raw-log binary
		// expressions whose operands would need parentheses; the retired
		// Python proxy rejected the same shape after injecting its user filter.
		if _, err := logqlv2.ParseExpr(rewritten); err != nil {
			return fmt.Errorf("%w: rewritten query does not parse: %w", ErrInvalidQuery, err)
		}
		v.Set(queryParam, rewritten)
		return nil
	}

	if len(pipelineRequired) > 0 {
		return fmt.Errorf("%w: label endpoints require indexed stream labels", ErrInvalidQuery)
	}
	rewritten, err := appendSelectorMatchers(paramValue, selectorRequired)
	if err != nil {
		return err
	}
	v.Set(queryParam, rewritten)
	return nil
}

// partitionRequiredMatchers splits authorization matchers into those that must
// be injected as stream-selector matchers and those that are applied as
// post-parse label filters. Only tenant_name is currently indexed on every
// stream; organization_id and user_id remain structured metadata for
// compatibility with existing collectors.
func partitionRequiredMatchers(required []*labels.Matcher) ([]*labels.Matcher, []*labels.Matcher) {
	var selector, pipeline []*labels.Matcher
	for _, matcher := range required {
		if matcher.Name == tenantLabel {
			selector = append(selector, matcher)
			continue
		}
		pipeline = append(pipeline, matcher)
	}
	return selector, pipeline
}

func escapePipelineMatcherValues(matchers []*labels.Matcher) []*labels.Matcher {
	escaped := make([]*labels.Matcher, 0, len(matchers))
	for _, matcher := range matchers {
		value := strings.ReplaceAll(matcher.Value, `\`, `\\`)
		value = strings.ReplaceAll(value, `"`, `\"`)
		escaped = append(escaped, &labels.Matcher{
			Type:  matcher.Type,
			Name:  matcher.Name,
			Value: value,
		})
	}
	return escaped
}

// combineLabelMatchers combines the query label matchers with matchers that
// must be enforced as stream selectors. A required matcher replaces any client
// matcher for the same label, so the injected tenant filter cannot be weakened
// or inverted by client-supplied matchers.
func combineLabelMatchers(queryMatchers, requiredMatchers []*labels.Matcher) []*labels.Matcher {
	queryMatchersMap := make(map[string]*labels.Matcher)
	for _, qm := range queryMatchers {
		queryMatchersMap[qm.Name] = qm
	}
	for _, rm := range requiredMatchers {
		queryMatchersMap[rm.Name] = rm
	}

	matchers := make([]*labels.Matcher, 0, len(queryMatchersMap))
	for _, m := range queryMatchersMap {
		matchers = append(matchers, m)
	}
	return matchers
}

func appendSelectorMatchers(query string, required []*labels.Matcher) (string, error) {
	selectors, err := streamSelectors(query)
	if err != nil {
		return "", err
	}
	if len(selectors) != 1 {
		return "", fmt.Errorf("%w: label endpoints require exactly one stream selector", ErrInvalidQuery)
	}

	selector := selectors[0]
	if strings.TrimSpace(query[:selector.start]) != "" ||
		strings.TrimSpace(query[selector.end+1:]) != "" {
		return "", fmt.Errorf("%w: label endpoints require a selector-only query", ErrInvalidQuery)
	}
	if len(required) == 0 {
		return query, nil
	}

	matchers := make([]string, 0, len(required))
	for _, matcher := range required {
		matchers = append(matchers, matcher.String())
	}
	return query[:selector.start+1] + strings.Join(matchers, ",") + "," + query[selector.start+1:], nil
}

func requiredSelector(required []*labels.Matcher) string {
	matchers := make([]string, 0, len(required))
	for _, matcher := range required {
		matchers = append(matchers, matcher.String())
	}
	return "{" + strings.Join(matchers, ",") + "}"
}

type streamSelectorPosition struct {
	start int
	end   int
}

func streamSelectors(query string) ([]streamSelectorPosition, error) {
	var selectors []streamSelectorPosition
	for index := 0; index < len(query); {
		switch query[index] {
		case '"', '\'', '`':
			end, err := quotedStringEnd(query, index)
			if err != nil {
				return nil, fmt.Errorf("%w: %w", ErrInvalidQuery, err)
			}
			index = end
		case '#':
			next, err := skipLineComment(query, index)
			if err != nil {
				return nil, fmt.Errorf("%w: %w", ErrInvalidQuery, err)
			}
			index = next
		case '/':
			if index+1 < len(query) && (query[index+1] == '/' || query[index+1] == '*') {
				next, err := skipComment(query, index)
				if err != nil {
					return nil, fmt.Errorf("%w: %w", ErrInvalidQuery, err)
				}
				index = next
				continue
			}
			index++
		case '{':
			selector, err := streamSelectorPositionEnd(query, index)
			if err != nil {
				return nil, fmt.Errorf("%w: %w", ErrInvalidQuery, err)
			}
			selectors = append(selectors, selector)
			index = selector.end + 1
		default:
			index++
		}
	}
	return selectors, nil
}

func quotedStringEnd(query string, start int) (int, error) {
	quote := query[start]
	for index := start + 1; index < len(query); index++ {
		if quote != '`' && query[index] == '\\' {
			index++
			continue
		}
		if query[index] == quote {
			return index + 1, nil
		}
	}
	return 0, errors.New("unterminated string")
}

func streamSelectorPositionEnd(query string, start int) (streamSelectorPosition, error) {
	for index := start + 1; index < len(query); index++ {
		switch query[index] {
		case '"', '\'', '`':
			end, err := quotedStringEnd(query, index)
			if err != nil {
				return streamSelectorPosition{}, err
			}
			index = end - 1
		case '#':
			next, err := skipLineComment(query, index)
			if err != nil {
				return streamSelectorPosition{}, err
			}
			index = next - 1
		case '/':
			if index+1 >= len(query) || (query[index+1] != '/' && query[index+1] != '*') {
				continue
			}
			next, err := skipComment(query, index)
			if err != nil {
				return streamSelectorPosition{}, err
			}
			index = next - 1
		case '}':
			return streamSelectorPosition{start: start, end: index}, nil
		case '{':
			return streamSelectorPosition{}, errors.New("nested stream selector")
		}
	}
	return streamSelectorPosition{}, errors.New("unterminated stream selector")
}

func skipComment(query string, start int) (int, error) {
	if start+1 < len(query) && query[start+1] == '/' {
		return skipLineComment(query, start)
	}
	if start+1 >= len(query) || query[start+1] != '*' {
		return 0, errors.New("invalid comment")
	}
	end := strings.Index(query[start+2:], "*/")
	if end < 0 {
		return 0, errors.New("unterminated block comment")
	}
	return start + 2 + end + 2, nil
}

func skipLineComment(query string, start int) (int, error) {
	newline := strings.IndexByte(query[start:], '\n')
	if newline < 0 {
		return len(query), nil
	}
	return start + newline + 1, nil
}
