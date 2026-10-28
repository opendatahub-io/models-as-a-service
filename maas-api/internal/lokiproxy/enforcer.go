// Package lokiproxy implements a read-only Loki proxy that restricts every
// query to the caller's organization.
//
// The label-matcher enforcement logic is adapted from observatorium-api's
// api/logs/v1/labels_enforcer.go (github.com/observatorium/api). Instead of
// observatorium's OPA-shaped authorization context, the required matchers are
// derived from maas-api's auth context (the trusted X-MaaS-Organization-Id
// header injected by the auth gateway).
package lokiproxy

import (
	"errors"
	"fmt"
	"net/url"

	logqlv2 "github.com/observatorium/api/logql/v2"
	"github.com/prometheus/prometheus/model/labels"
)

const queryParam = "query"

// ErrInvalidQuery is returned when the client supplied a query that cannot be
// parsed as a LogQL expression. The caller is expected to surface it as a 400.
var ErrInvalidQuery = errors.New("invalid LogQL query")

// enforceQueryMatchers rewrites the "query" parameter in v so that every stream
// selector of the LogQL expression contains the required authorization
// matchers. It mirrors observatorium-api's combineLabelMatchers semantics: a
// required matcher silently overwrites any client-supplied matcher for the same
// label that is not already covered by it, so the injected organization filter
// always wins.
//
// queryEndpoint marks endpoints where an empty query is left untouched (Loki
// rejects those requests itself); on the label endpoints an empty query is
// replaced with a selector built from the required matchers so that label
// listings are organization-scoped as well.
func enforceQueryMatchers(v url.Values, required []*labels.Matcher, queryEndpoint bool) error {
	paramValue := v.Get(queryParam)
	if paramValue == "" {
		if queryEndpoint {
			return nil
		}

		expr := &logqlv2.StreamMatcherExpr{}
		expr.SetMatchers(required)
		v.Set(queryParam, expr.String())
		return nil
	}

	expr, err := logqlv2.ParseExpr(paramValue)
	if err != nil {
		return fmt.Errorf("%w: %w", ErrInvalidQuery, err)
	}

	expr.Walk(func(e any) {
		switch le := e.(type) {
		case *logqlv2.LogQueryExpr:
			le.SetMatchers(combineLabelMatchers(le.Matchers(), required))
		case *logqlv2.StreamMatcherExpr:
			le.SetMatchers(combineLabelMatchers(le.Matchers(), required))
		}
	})

	v.Set(queryParam, expr.String())
	return nil
}

// combineLabelMatchers combines the query label matchers with the required
// authorization matchers. A required matcher always replaces any client
// matcher for the same label (silent overwrite), so the injected organization
// filter cannot be weakened or inverted by client-supplied matchers —
// including negative matchers such as organization_id!="<org>".
//
// Adapted from observatorium-api's combineLabelMatchers, which keeps a client
// matcher whose value is matched by the required one. That optimization is
// unsafe for negative client matchers, so this variant always overwrites.
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
