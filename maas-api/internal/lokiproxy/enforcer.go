// Package lokiproxy implements a read-only Loki proxy that restricts every
// query to the caller's organization.
package lokiproxy

import (
	"errors"
	"fmt"
	"net/url"
	"strings"

	"github.com/prometheus/prometheus/model/labels"
)

const queryParam = "query"

// maxQueryLength preserves the retired Python proxy's query-size limit.
const maxQueryLength = 32 * 1024

// ErrInvalidQuery is returned when the client supplied a query that cannot be
// parsed as a LogQL expression. The caller is expected to surface it as a 400.
var ErrInvalidQuery = errors.New("invalid LogQL query")

// enforceQueryMatchers rewrites the "query" parameter in v so that every log
// query is restricted by the required authorization matchers. On query and
// query_range endpoints the matchers are appended as label-filter stages,
// preserving compatibility with collectors that expose identity fields as
// Loki structured metadata rather than indexed stream labels. On label
// endpoints they are injected into stream selectors because those Loki APIs
// require a selector.
func enforceQueryMatchers(v url.Values, required []*labels.Matcher, queryEndpoint bool) error {
	paramValue := v.Get(queryParam)
	if len(paramValue) > maxQueryLength {
		return fmt.Errorf("%w: query exceeds %d bytes", ErrInvalidQuery, maxQueryLength)
	}
	if paramValue == "" {
		if queryEndpoint {
			return nil
		}
		if len(required) == 0 {
			v.Del(queryParam)
			return nil
		}

		v.Set(queryParam, requiredSelector(required))
		return nil
	}

	if queryEndpoint {
		rewritten, err := appendPipelineMatchers(paramValue, required)
		if err != nil {
			return err
		}
		v.Set(queryParam, rewritten)
		return nil
	}

	rewritten, err := appendSelectorMatchers(paramValue, required)
	if err != nil {
		return err
	}
	v.Set(queryParam, rewritten)
	return nil
}

func appendPipelineMatchers(query string, required []*labels.Matcher) (string, error) {
	if len(required) == 0 {
		return query, nil
	}

	selectors, err := streamSelectors(query)
	if err != nil {
		return "", err
	}
	if len(selectors) == 0 {
		return "", fmt.Errorf("%w: query has no stream selector", ErrInvalidQuery)
	}

	filter := requiredFilter(required)
	rewritten := query
	for index := len(selectors) - 1; index >= 0; index-- {
		insertionPoint := selectors[index].end + 1
		rewritten = rewritten[:insertionPoint] + " " + filter + rewritten[insertionPoint:]
	}

	if err := validatePipelineMatchers(rewritten, filter, len(selectors)); err != nil {
		return "", err
	}
	return rewritten, nil
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

func requiredFilter(required []*labels.Matcher) string {
	matchers := make([]string, 0, len(required))
	for _, matcher := range required {
		matchers = append(matchers, matcher.String())
	}
	return "| " + strings.Join(matchers, " and ")
}

func requiredSelector(required []*labels.Matcher) string {
	matchers := make([]string, 0, len(required))
	for _, matcher := range required {
		matchers = append(matchers, matcher.String())
	}
	return "{" + strings.Join(matchers, ",") + "}"
}

func validatePipelineMatchers(query, filter string, selectorCount int) error {
	selectors, err := streamSelectors(query)
	if err != nil {
		return err
	}
	if len(selectors) != selectorCount {
		return fmt.Errorf("%w: query rewrite changed stream selector count", ErrInvalidQuery)
	}

	requiredPrefix := " " + filter
	for _, selector := range selectors {
		if !strings.HasPrefix(query[selector.end+1:], requiredPrefix) {
			return fmt.Errorf("%w: query rewrite did not protect every stream selector", ErrInvalidQuery)
		}
		continuation := selector.end + 1 + len(requiredPrefix)
		if startsWithBooleanOperator(query, continuation) {
			return fmt.Errorf("%w: identity filter cannot be extended by a boolean operator", ErrInvalidQuery)
		}
	}
	return nil
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

func startsWithBooleanOperator(query string, start int) bool {
	for start < len(query) {
		switch query[start] {
		case ' ', '\t', '\r', '\n':
			start++
			continue
		case '#':
			next, err := skipLineComment(query, start)
			if err != nil {
				return false
			}
			start = next
			continue
		case '/':
			if start+1 < len(query) && (query[start+1] == '/' || query[start+1] == '*') {
				next, err := skipComment(query, start)
				if err != nil {
					return false
				}
				start = next
				continue
			}
		}
		break
	}

	for _, operator := range []string{"or", "and", "unless"} {
		end := start + len(operator)
		if len(query[start:]) >= len(operator) && strings.EqualFold(query[start:end], operator) &&
			(end >= len(query) || !isWordByte(query[end])) {
			return true
		}
	}
	return false
}

func isWordByte(value byte) bool {
	return value == '_' || (value >= 'a' && value <= 'z') || (value >= 'A' && value <= 'Z') || (value >= '0' && value <= '9')
}
