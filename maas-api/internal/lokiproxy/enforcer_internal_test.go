package lokiproxy

import (
	"net/url"
	"sort"
	"strconv"
	"strings"
	"testing"

	logqlv2 "github.com/observatorium/api/logql/v2"
	"github.com/prometheus/prometheus/model/labels"
	"github.com/stretchr/testify/require"
)

func orgMatcher() *labels.Matcher {
	return &labels.Matcher{Type: labels.MatchEqual, Name: "organization_id", Value: "1234"}
}

// enforcedQuery applies enforceQueryMatchers and returns the resulting "query"
// parameter, mirroring the rewrite the proxy performs before forwarding.
func enforcedQuery(t *testing.T, initial url.Values, required []*labels.Matcher, queryEndpoint bool) string {
	t.Helper()

	v := url.Values{}
	for key, values := range initial {
		v[key] = append([]string{}, values...)
	}
	require.NoError(t, enforceQueryMatchers(v, required, queryEndpoint))
	return v.Get(queryParam)
}

// selectorMatchers parses a LogQL expression and returns the matchers of every
// stream selector found in it (sorted for stable comparison). Every selector
// is visited through its StreamMatcherExpr, which yields each matcher exactly
// once.
func selectorMatchers(t *testing.T, query string) []string {
	t.Helper()

	expr, err := logqlv2.ParseExpr(query)
	require.NoError(t, err)

	var matchers []string
	expr.Walk(func(e any) {
		if le, ok := e.(*logqlv2.StreamMatcherExpr); ok {
			for _, m := range le.Matchers() {
				matchers = append(matchers, m.String())
			}
		}
	})
	sort.Strings(matchers)
	return matchers
}

func TestEnforceQueryMatchersQueryEndpoint(t *testing.T) {
	t.Parallel()
	tt := []struct {
		desc  string
		query string
	}{
		{
			desc:  "query with no organization matcher is extended",
			query: `{app="api"}`,
		},
		{
			desc:  "conflicting organization selector is restricted by an injected filter",
			query: `{app="api", organization_id="other-org"}`,
		},
		{
			desc:  "client matcher on the organization label is restricted by an injected filter",
			query: `{organization_id="1234", app="api"}`,
		},
		{
			desc:  "regex organization selector is restricted by an injected filter",
			query: `{organization_id=~".*"}`,
		},
		{
			desc:  "pipeline expression selector is enforced",
			query: `{app="api"} | json | foo="bar"`,
		},
		{
			desc:  "metric query selector is enforced",
			query: `sum(count_over_time({app="api"}[5m]))`,
		},
		{
			desc:  "binary expression selectors are enforced",
			query: `sum(rate({app="a"}[1m])) or sum(rate({app="b"}[1m]))`,
		},
	}

	for _, tc := range tt {
		t.Run(tc.desc, func(t *testing.T) {
			t.Parallel()

			query := enforcedQuery(t, url.Values{queryParam: []string{tc.query}}, []*labels.Matcher{orgMatcher()}, true)
			require.Contains(t, query, `| organization_id="1234"`)
		})
	}
}

func TestEnforceQueryMatchersQueryEndpointPreservesUnindexedIdentityLabels(t *testing.T) {
	t.Parallel()

	query := enforcedQuery(t, url.Values{queryParam: []string{`{app="api"}`}}, []*labels.Matcher{orgMatcher()}, true)

	require.Equal(t, []string{`app="api"`}, selectorMatchers(t, query))
	require.Contains(t, query, `| organization_id="1234"`)
}

func TestEnforceQueryMatchersEscapesIdentityValuesAndCoversBranches(t *testing.T) {
	t.Parallel()

	username := `user"\\`
	organizationID := `org"x\`
	required := []*labels.Matcher{
		{Type: labels.MatchEqual, Name: "user_id", Value: username},
		{Type: labels.MatchEqual, Name: "organization_id", Value: organizationID},
	}
	query := enforcedQuery(t, url.Values{queryParam: []string{
		`sum(count_over_time({app="a"}[1m])) or sum(count_over_time({app="b"}[1m]))`,
	}}, required, true)
	filter := `| user_id=` + strconv.Quote(username) +
		` and organization_id=` + strconv.Quote(organizationID)

	require.Contains(t, query, filter)
	requirePipelineFilterOnEveryLogQuery(t, query, filter)
}

func TestEnforceQueryMatchersAppliesAuthorizationBeforeCallerParsers(t *testing.T) {
	t.Parallel()

	query := `{app="api"} | json | organization_id="attacker"`
	rewritten := enforcedQuery(t, url.Values{queryParam: []string{query}}, []*labels.Matcher{orgMatcher()}, true)

	require.Equal(t, `{app="api"} | organization_id="1234" | json | organization_id="attacker"`, rewritten)
}

func requirePipelineFilterOnEveryLogQuery(t *testing.T, query, filter string) {
	t.Helper()

	expr, err := logqlv2.ParseExpr(query)
	require.NoError(t, err)

	logQueries := 0
	expr.Walk(func(e any) {
		if _, ok := e.(*logqlv2.LogQueryExpr); ok {
			logQueries++
		}
	})
	require.Positive(t, logQueries, "query must contain at least one log selector")
	require.Equal(t, logQueries, strings.Count(query, filter), "every log query branch must contain the identity filter")
}

func TestEnforceQueryMatchersQueryEndpointEmptyQuery(t *testing.T) {
	// On query endpoints an empty query is left untouched; Loki rejects it.
	v := url.Values{"limit": []string{"10"}}
	require.NoError(t, enforceQueryMatchers(v, []*labels.Matcher{orgMatcher()}, true))
	require.Empty(t, v.Get(queryParam))
	require.Equal(t, "10", v.Get("limit"))
}

func TestEnforceQueryMatchersLabelEndpoints(t *testing.T) {
	t.Parallel()
	tt := []struct {
		desc     string
		query    string
		expected []string
	}{
		{
			desc:     "empty query becomes an organization selector",
			query:    "",
			expected: []string{`organization_id="1234"`},
		},
		{
			desc:     "client selector is combined with organization matcher",
			query:    `{app="api"}`,
			expected: []string{`app="api"`, `organization_id="1234"`},
		},
	}

	for _, tc := range tt {
		t.Run(tc.desc, func(t *testing.T) {
			t.Parallel()

			query := enforcedQuery(t, url.Values{queryParam: []string{tc.query}}, []*labels.Matcher{orgMatcher()}, false)
			require.Equal(t, tc.expected, selectorMatchers(t, query))
		})
	}
}

// The user-scoped view injects both the organization and the user matcher as
// label filters; conflicting client selectors cannot weaken the injected filters.
func TestEnforceQueryMatchersUserScoped(t *testing.T) {
	t.Parallel()

	required := []*labels.Matcher{
		orgMatcher(),
		{Type: labels.MatchEqual, Name: "user_id", Value: "user-abc"},
	}

	tt := []struct {
		desc  string
		query string
	}{
		{
			desc:  "plain selector gains both matchers",
			query: `{app="api"}`,
		},
		{
			desc:  "conflicting user matcher is restricted by an injected filter",
			query: `{user_id="victim", organization_id="other"}`,
		},
		{
			desc:  "negative user matcher is restricted by an injected filter",
			query: `{user_id!="user-abc"}`,
		},
		{
			desc:  "regex user matcher is restricted by an injected filter",
			query: `{user_id=~".*"}`,
		},
	}

	for _, tc := range tt {
		t.Run(tc.desc, func(t *testing.T) {
			t.Parallel()

			query := enforcedQuery(t, url.Values{queryParam: []string{tc.query}}, required, true)
			require.Contains(t, query, `| organization_id="1234" and user_id="user-abc"`)
		})
	}
}

func TestEnforceQueryMatchersInvalidQuery(t *testing.T) {
	v := url.Values{queryParam: []string{`{app="unclosed`}}
	err := enforceQueryMatchers(v, []*labels.Matcher{orgMatcher()}, true)
	require.ErrorIs(t, err, ErrInvalidQuery)
}

func TestEnforceQueryMatchersPersonalDashboardGrammar(t *testing.T) {
	t.Parallel()

	required := []*labels.Matcher{
		orgMatcher(),
		{Type: labels.MatchEqual, Name: "user_id", Value: "user-abc"},
	}
	queries := []string{
		`sum(sum_over_time({service_name="models-as-a-service", subscription=~".*", model=~".*", response_type="hit"}` +
			` | label_format __ak_sel="(.*)" | label_format __ak_keep="{{if eq .__ak_sel \"(.*)\"}}true{{else if (count (printf \"^%s$\" .__ak_sel) .key_name)}}true{{else}}false{{end}}"` +
			` | __ak_keep="true" | unwrap tokens_total [5m])) or vector(0)`,
		`sum by (model, subscription, key_name) (sum_over_time(` +
			`{service_name="models-as-a-service", subscription=~".*", subscription!="-", model=~".*"}` +
			` | label_format __ak_sel="(.*)" | label_format __ak_keep="true"` +
			` | label_format target_tokens=` + "`{{ if eq .response_type \"hit\" }}{{ .tokens_total }}{{ else }}0{{ end }}`" +
			` | unwrap target_tokens [5m]))`,
	}

	for _, query := range queries {
		rewritten := enforcedQuery(t, url.Values{queryParam: []string{query}}, required, true)
		require.Contains(t, rewritten, `| organization_id="1234" and user_id="user-abc"`)
	}
}

func TestEnforceQueryMatchersRejectsOversizedQuery(t *testing.T) {
	v := url.Values{queryParam: []string{strings.Repeat("a", maxQueryLength+1)}}
	err := enforceQueryMatchers(v, []*labels.Matcher{orgMatcher()}, true)
	require.ErrorIs(t, err, ErrInvalidQuery)
}

// The injected filter must always be present alongside client-supplied
// matchers for the same label, including regex and pipeline forms. A
// conflicting client selector and the required filter both have to match, so
// the client selector cannot weaken authorization.
func TestInjectedMatcherAlwaysWins(t *testing.T) {
	t.Parallel()

	required := []*labels.Matcher{orgMatcher()}

	queries := []string{
		`{organization_id="other"}`,
		`{organization_id=~".*"}`,
		`{organization_id!~"12.*"}`,
		`{organization_id!="1234"} | json`,
		`sum(count_over_time({organization_id=~"other|12"}[5m]))`,
	}

	for _, query := range queries {
		enforced := enforcedQuery(t, url.Values{queryParam: []string{query}}, required, true)
		require.Contains(t, enforced, `| organization_id="1234"`, "query: %s", query)
	}
}

func TestAppendPipelineMatchersPreservesCallerText(t *testing.T) {
	t.Parallel()

	query := `label_replace(count_over_time({app="a"}[5m]), "foo", "injected\" or vector(0)", "foo", ".*")`
	rewritten, err := appendPipelineMatchers(query, []*labels.Matcher{orgMatcher()})
	require.NoError(t, err)

	expected := `label_replace(count_over_time({app="a"} | organization_id="1234"[5m]), ` +
		`"foo", "injected\" or vector(0)", "foo", ".*")`
	require.Equal(t, expected, rewritten)
	require.NoError(t, validatePipelineMatchers(rewritten, `| organization_id="1234"`, 1))
}

func TestAppendPipelineMatchersProtectsEveryNestedBranch(t *testing.T) {
	t.Parallel()

	query := `sum(count_over_time({app="a"}[5m])) or sum(count_over_time({app="b"}[5m]))`
	rewritten, err := appendPipelineMatchers(query, []*labels.Matcher{orgMatcher()})
	require.NoError(t, err)

	expected := `sum(count_over_time({app="a"} | organization_id="1234"[5m])) or ` +
		`sum(count_over_time({app="b"} | organization_id="1234"[5m]))`
	require.Equal(t, expected, rewritten)
}

func TestAppendPipelineMatchersRejectsSelectorlessQuery(t *testing.T) {
	t.Parallel()

	_, err := appendPipelineMatchers(`vector(0)`, []*labels.Matcher{orgMatcher()})
	require.ErrorIs(t, err, ErrInvalidQuery)
}

func TestAppendPipelineMatchersSkipsComments(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name     string
		query    string
		expected string
	}{
		{
			name:     "hash comment inside selector",
			query:    "{app=\"a\" # }\n} | json",
			expected: "{app=\"a\" # }\n} | organization_id=\"1234\" | json",
		},
		{
			name:     "line comment after selector",
			query:    "{app=\"a\"} // }\n| json",
			expected: "{app=\"a\"} | organization_id=\"1234\" // }\n| json",
		},
		{
			name:     "block comment after selector",
			query:    "{app=\"a\"} /* } */ | json",
			expected: "{app=\"a\"} | organization_id=\"1234\" /* } */ | json",
		},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			t.Parallel()

			rewritten, err := appendPipelineMatchers(test.query, []*labels.Matcher{orgMatcher()})
			require.NoError(t, err)
			require.Equal(t, test.expected, rewritten)
		})
	}
}

func TestAppendPipelineMatchersDoesNotUseUnboundedParser(t *testing.T) {
	t.Parallel()

	query := `{app="a"} | logfmt --strict`
	rewritten, err := appendPipelineMatchers(query, []*labels.Matcher{orgMatcher()})
	require.NoError(t, err)
	require.Equal(t, `{app="a"} | organization_id="1234" | logfmt --strict`, rewritten)
}

func TestAppendPipelineMatchersRejectsBooleanFilterContinuation(t *testing.T) {
	t.Parallel()

	queries := []string{
		`{app="a"} or {app="b"}`,
		`{app="a"} or user_id="victim"`,
		`{app="a"} and user_id="victim"`,
		`{app="a"} unless user_id="victim"`,
		`{app="a"} /* ignored */ or user_id="victim"`,
		`{app="a"} OR user_id="victim"`,
		`{app="a"} And user_id="victim"`,
		`{app="a"} UnLeSs user_id="victim"`,
	}
	for _, query := range queries {
		_, err := appendPipelineMatchers(query, []*labels.Matcher{orgMatcher()})
		require.ErrorIs(t, err, ErrInvalidQuery, "query: %s", query)
		require.ErrorContains(t, err, "boolean operator", "query: %s", query)
	}
}

func TestAppendPipelineMatchersSupportsParenthesizedPredicates(t *testing.T) {
	t.Parallel()

	query := `{app="a"} | (foo="bar" and baz="qux")`
	rewritten, err := appendPipelineMatchers(query, []*labels.Matcher{orgMatcher()})
	require.NoError(t, err)
	require.Equal(t, `{app="a"} | organization_id="1234" | (foo="bar" and baz="qux")`, rewritten)
}

func TestEnforceQueryMatchersLabelEndpointRequiresSelectorOnlyQuery(t *testing.T) {
	t.Parallel()

	tests := []string{
		`{app="api"} | json`,
		`sum(count_over_time({app="api"}[5m]))`,
		`{app="api"} {app="other"}`,
	}
	for _, query := range tests {
		err := enforceQueryMatchers(url.Values{queryParam: []string{query}}, []*labels.Matcher{orgMatcher()}, false)
		require.ErrorIs(t, err, ErrInvalidQuery, "query: %s", query)
	}
}

func TestEnforceQueryMatchersLabelEndpointPreservesRepeatedMatchers(t *testing.T) {
	t.Parallel()

	v := url.Values{queryParam: []string{`{app="a",app=~"a.*"}`}}
	require.NoError(t, enforceQueryMatchers(v, []*labels.Matcher{orgMatcher()}, false))
	require.Equal(t, `{organization_id="1234",app="a",app=~"a.*"}`, v.Get(queryParam))
}

func TestEnforceQueryMatchersLabelEndpointSupportsQuotedBraces(t *testing.T) {
	t.Parallel()

	v := url.Values{queryParam: []string{`{app="a{b}"}`}}
	require.NoError(t, enforceQueryMatchers(v, []*labels.Matcher{orgMatcher()}, false))
	require.Equal(t, `{organization_id="1234",app="a{b}"}`, v.Get(queryParam))
}

func TestEnforceQueryMatchersLabelEndpointUnscopedEmptyQuery(t *testing.T) {
	t.Parallel()

	v := url.Values{queryParam: []string{""}}
	require.NoError(t, enforceQueryMatchers(v, nil, false))
	require.Empty(t, v.Get(queryParam))
}
