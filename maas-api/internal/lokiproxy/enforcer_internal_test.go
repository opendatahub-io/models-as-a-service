package lokiproxy

import (
	"net/url"
	"sort"
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
		desc     string
		query    string
		expected []string
	}{
		{
			desc:     "query with no organization matcher is extended",
			query:    `{app="api"}`,
			expected: []string{`app="api"`, `organization_id="1234"`},
		},
		{
			desc:     "conflicting organization matcher is overwritten",
			query:    `{app="api", organization_id="other-org"}`,
			expected: []string{`app="api"`, `organization_id="1234"`},
		},
		{
			desc:     "client matcher on the organization label is always overwritten",
			query:    `{organization_id="1234", app="api"}`,
			expected: []string{`app="api"`, `organization_id="1234"`},
		},
		{
			desc:     "regex organization matcher is overwritten",
			query:    `{organization_id=~".*"}`,
			expected: []string{`organization_id="1234"`},
		},
		{
			desc:     "pipeline expression selector is enforced",
			query:    `{app="api"} | json | foo="bar"`,
			expected: []string{`app="api"`, `organization_id="1234"`},
		},
		{
			desc:     "metric query selector is enforced",
			query:    `sum(count_over_time({app="api"}[5m]))`,
			expected: []string{`app="api"`, `organization_id="1234"`},
		},
		{
			desc:     "binary expression selectors are enforced",
			query:    `sum(rate({app="a"}[1m])) or sum(rate({app="b"}[1m]))`,
			expected: []string{`app="a"`, `app="b"`, `organization_id="1234"`, `organization_id="1234"`},
		},
	}

	for _, tc := range tt {
		t.Run(tc.desc, func(t *testing.T) {
			t.Parallel()

			query := enforcedQuery(t, url.Values{queryParam: []string{tc.query}}, []*labels.Matcher{orgMatcher()}, true)
			require.Equal(t, tc.expected, selectorMatchers(t, query))
		})
	}
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

// The user-scoped view injects both the organization and the user matcher;
// conflicting client matchers for either label must be overridden.
func TestEnforceQueryMatchersUserScoped(t *testing.T) {
	t.Parallel()

	required := []*labels.Matcher{
		orgMatcher(),
		{Type: labels.MatchEqual, Name: "user_id", Value: "user-abc"},
	}

	tt := []struct {
		desc     string
		query    string
		expected []string
	}{
		{
			desc:     "plain selector gains both matchers",
			query:    `{app="api"}`,
			expected: []string{`app="api"`, `organization_id="1234"`, `user_id="user-abc"`},
		},
		{
			desc:     "conflicting user matcher is overwritten",
			query:    `{user_id="victim", organization_id="other"}`,
			expected: []string{`organization_id="1234"`, `user_id="user-abc"`},
		},
		{
			desc:     "negative user matcher is overwritten",
			query:    `{user_id!="user-abc"}`,
			expected: []string{`organization_id="1234"`, `user_id="user-abc"`},
		},
		{
			desc:     "regex user matcher is overwritten",
			query:    `{user_id=~".*"}`,
			expected: []string{`organization_id="1234"`, `user_id="user-abc"`},
		},
	}

	for _, tc := range tt {
		t.Run(tc.desc, func(t *testing.T) {
			t.Parallel()

			query := enforcedQuery(t, url.Values{queryParam: []string{tc.query}}, required, true)
			require.Equal(t, tc.expected, selectorMatchers(t, query))
		})
	}
}

func TestEnforceQueryMatchersInvalidQuery(t *testing.T) {
	v := url.Values{queryParam: []string{`{app="unclosed`}}
	err := enforceQueryMatchers(v, []*labels.Matcher{orgMatcher()}, true)
	require.ErrorIs(t, err, ErrInvalidQuery)
}

func TestCombineLabelMatchers(t *testing.T) {
	t.Parallel()
	tt := []struct {
		desc             string
		queryMatchers    []*labels.Matcher
		requiredMatchers []*labels.Matcher
		expected         []string
	}{
		{
			desc:             "required matcher added when label absent",
			queryMatchers:    []*labels.Matcher{{Type: labels.MatchEqual, Name: "app", Value: "api"}},
			requiredMatchers: []*labels.Matcher{orgMatcher()},
			expected:         []string{`app="api"`, `organization_id="1234"`},
		},
		{
			desc:             "conflicting matcher silently overwritten",
			queryMatchers:    []*labels.Matcher{{Type: labels.MatchEqual, Name: "organization_id", Value: "other"}},
			requiredMatchers: []*labels.Matcher{orgMatcher()},
			expected:         []string{`organization_id="1234"`},
		},
		{
			desc:             "exact match on allowed value also overwritten",
			queryMatchers:    []*labels.Matcher{orgMatcher()},
			requiredMatchers: []*labels.Matcher{orgMatcher()},
			expected:         []string{`organization_id="1234"`},
		},
		{
			desc:             "regex matcher overwritten",
			queryMatchers:    []*labels.Matcher{{Type: labels.MatchRegexp, Name: "organization_id", Value: ".*"}},
			requiredMatchers: []*labels.Matcher{orgMatcher()},
			expected:         []string{`organization_id="1234"`},
		},
		{
			desc:             "negative matcher overwritten",
			queryMatchers:    []*labels.Matcher{{Type: labels.MatchNotEqual, Name: "organization_id", Value: "1234"}},
			requiredMatchers: []*labels.Matcher{orgMatcher()},
			expected:         []string{`organization_id="1234"`},
		},
	}

	for _, tc := range tt {
		t.Run(tc.desc, func(t *testing.T) {
			t.Parallel()

			combined := combineLabelMatchers(tc.queryMatchers, tc.requiredMatchers)
			got := make([]string, 0, len(combined))
			for _, m := range combined {
				got = append(got, m.String())
			}
			sort.Strings(got)
			sort.Strings(tc.expected)
			require.Equal(t, tc.expected, got)
		})
	}
}

// The injected matcher must always win over client-supplied matchers for the
// same label, including regex and pipeline forms (defense in depth on top of
// the query endpoint tests above).
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
		matchers := selectorMatchers(t, enforced)
		require.Contains(t, matchers, `organization_id="1234"`, "query: %s", query)
	}
}
