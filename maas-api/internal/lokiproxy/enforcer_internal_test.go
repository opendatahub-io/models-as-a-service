package lokiproxy

import (
	"net/url"
	"strings"
	"testing"

	logqlv2 "github.com/observatorium/api/logql/v2"
	"github.com/prometheus/prometheus/model/labels"
	"github.com/stretchr/testify/require"
)

func requiredTenantMatcher() *labels.Matcher {
	return &labels.Matcher{Type: labels.MatchEqual, Name: tenantLabel, Value: "tenant-a"}
}

func requiredOrganizationMatcher() *labels.Matcher {
	return &labels.Matcher{Type: labels.MatchEqual, Name: organizationLabel, Value: "1234"}
}

func requiredUserMatcher() *labels.Matcher {
	return &labels.Matcher{Type: labels.MatchEqual, Name: userLabel, Value: "user-abc"}
}

func enforcedQuery(t *testing.T, query string, required []*labels.Matcher, queryEndpoint bool) string {
	t.Helper()

	values := url.Values{queryParam: []string{query}}
	require.NoError(t, enforceQueryMatchers(values, required, queryEndpoint))
	return values.Get(queryParam)
}

func TestEnforceQueryMatchersQueryEndpoint(t *testing.T) {
	t.Parallel()

	required := []*labels.Matcher{
		requiredTenantMatcher(),
		requiredOrganizationMatcher(),
		requiredUserMatcher(),
	}
	expectedFilter := `| organization_id = "1234" and user_id = "user-abc"`

	tests := []struct {
		name  string
		query string
	}{
		{
			name:  "plain selector",
			query: `{app="api"}`,
		},
		{
			name:  "existing identity selectors",
			query: `{app="api", organization_id="other", user_id="victim"}`,
		},
		{
			name:  "pipeline expression",
			query: `{app="api"} | json | level="info"`,
		},
		{
			name:  "metric expression",
			query: `sum(count_over_time({app="api"}[5m]))`,
		},
		{
			name:  "binary expression",
			query: `sum(rate({app="a"}[1m])) or sum(rate({app="b"}[1m]))`,
		},
		{
			name: "dashboard variable pipeline",
			query: `count by (api_key) (count_over_time({service_name="models-as-a-service"} ` +
				`| user_id!="" | user_id!="-" | label_format api_key="{{.key_name}} ({{.user_id}})" ` +
				`| keep api_key [5m]))`,
		},
		{
			name: "dashboard raw-string pipeline",
			query: `sum by (target_tokens) (sum_over_time({service_name="models-as-a-service"} ` +
				"| label_format target_tokens=`{{ if eq .response_type \"hit\" }}{{ .tokens_total }}{{ else }}0{{ end }}`" +
				` | unwrap target_tokens [5m]))`,
		},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			t.Parallel()

			query := enforcedQuery(t, test.query, required, true)
			require.Contains(t, query, `tenant_name="tenant-a"`)
			require.Equal(
				t,
				strings.Count(query, `tenant_name="tenant-a"`),
				strings.Count(query, " "+expectedFilter),
				"query: %s",
				query,
			)
		})
	}
}

func TestEnforceQueryMatchersRejectsRawLogBinaryExpression(t *testing.T) {
	t.Parallel()

	values := url.Values{queryParam: []string{`{app="a"} or {app="b"}`}}
	err := enforceQueryMatchers(
		values,
		[]*labels.Matcher{
			requiredTenantMatcher(),
			requiredOrganizationMatcher(),
			requiredUserMatcher(),
		},
		true,
	)
	require.ErrorIs(t, err, ErrInvalidQuery)
	require.ErrorContains(t, err, "rewritten query does not parse")
}

func TestEnforceQueryMatchersRejectsRendererInjection(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name  string
		query string
		valid bool
	}{
		{
			name:  "raw string breakout",
			valid: false,
			query: `sum(count_over_time({service_name="models-as-a-service"} | x = ` + "`" +
				`a") [1h])) or count by (tenant_name, line) (count_over_time({service_name=~".+"} ` +
				`| label_format line="{{__line__}}" [1h])) #@` + "`" + ` [1h]))`,
		},
		{
			name:  "label_replace parameter breakout",
			valid: false,
			query: `label_replace(sum(count_over_time({service_name="models-as-a-service"}[1h])), ` +
				`"destination", "$1", "source", ".*\") or count by (tenant_name) ` +
				`(count_over_time({service_name=~\".+\"}[1h])) #")`,
		},
		{
			name:  "benign raw string containing a double quote",
			valid: true,
			query: `{app="api"} | line_format ` + "`" + `{{ if eq .level \"info\" }}{{ .message }}{{ end }}` + "`",
		},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			t.Parallel()

			values := url.Values{queryParam: []string{test.query}}
			err := enforceQueryMatchers(values, []*labels.Matcher{
				requiredTenantMatcher(),
				requiredOrganizationMatcher(),
				requiredUserMatcher(),
			}, true)
			if test.valid {
				require.NoError(t, err)
				_, parseErr := logqlv2.ParseExpr(values.Get(queryParam))
				require.NoError(t, parseErr, "query: %s", values.Get(queryParam))
				return
			}

			require.ErrorIs(t, err, ErrInvalidQuery)
			require.ErrorContains(t, err, "rewritten query failed selector enforcement")
		})
	}
}

func TestEnforceQueryMatchersEscapesPipelineMatcherValues(t *testing.T) {
	t.Parallel()

	maliciousUsername := `user-" or organization_id!="1234`
	query := enforcedQuery(
		t,
		`{app="api"}`,
		[]*labels.Matcher{
			requiredTenantMatcher(),
			requiredOrganizationMatcher(),
			{Type: labels.MatchEqual, Name: userLabel, Value: maliciousUsername},
		},
		true,
	)

	_, err := logqlv2.ParseExpr(query)
	require.NoError(t, err, "query: %s", query)
	require.Contains(t, query, `user_id = "user-\" or organization_id!=\"1234"`)
}

func TestEnforceQueryMatchersQueryEndpointEmptyAndOversized(t *testing.T) {
	values := url.Values{"limit": []string{"100"}}
	err := enforceQueryMatchers(values, []*labels.Matcher{requiredTenantMatcher()}, true)
	require.ErrorIs(t, err, ErrInvalidQuery)
	require.ErrorContains(t, err, "empty query")
	require.Equal(t, "100", values.Get("limit"))

	values = url.Values{queryParam: []string{strings.Repeat("a", maxQueryLength+1)}}
	err = enforceQueryMatchers(values, []*labels.Matcher{requiredTenantMatcher()}, true)
	require.ErrorIs(t, err, ErrInvalidQuery)
	require.ErrorContains(t, err, "query too long")
}

func TestEnforceQueryMatchersLabelEndpoints(t *testing.T) {
	t.Parallel()

	t.Run("empty query uses the tenant selector", func(t *testing.T) {
		t.Parallel()
		require.Equal(
			t,
			`{tenant_name="tenant-a"}`,
			enforcedQuery(t, "", []*labels.Matcher{requiredTenantMatcher()}, false),
		)
	})

	t.Run("selector-only query gains the tenant matcher", func(t *testing.T) {
		t.Parallel()
		query := enforcedQuery(
			t,
			`{app="api"}`,
			[]*labels.Matcher{requiredTenantMatcher()},
			false,
		)
		require.Contains(t, query, `app="api"`)
		require.Contains(t, query, `tenant_name="tenant-a"`)
	})

	t.Run("identity filters are rejected", func(t *testing.T) {
		t.Parallel()
		for _, query := range []string{"", `{app="api"}`} {
			values := url.Values{queryParam: []string{query}}
			err := enforceQueryMatchers(
				values,
				[]*labels.Matcher{requiredTenantMatcher(), requiredOrganizationMatcher()},
				false,
			)
			require.ErrorIs(t, err, ErrInvalidQuery)
			require.ErrorContains(t, err, "label endpoints require indexed stream labels")
		}
	})

	t.Run("access-mode matrix", func(t *testing.T) {
		t.Parallel()
		tests := []struct {
			name     string
			required []*labels.Matcher
			allowed  bool
		}{
			{
				name:     "tenant admin",
				required: []*labels.Matcher{requiredTenantMatcher()},
				allowed:  true,
			},
			{
				name: "tenant user",
				required: []*labels.Matcher{
					requiredTenantMatcher(),
					requiredUserMatcher(),
				},
			},
			{
				name: "organization admin",
				required: []*labels.Matcher{
					requiredTenantMatcher(),
					requiredOrganizationMatcher(),
				},
			},
			{
				name: "organization user",
				required: []*labels.Matcher{
					requiredTenantMatcher(),
					requiredOrganizationMatcher(),
					requiredUserMatcher(),
				},
			},
		}
		for _, test := range tests {
			t.Run(test.name, func(t *testing.T) {
				t.Parallel()
				err := enforceQueryMatchers(url.Values{}, test.required, false)
				if test.allowed {
					require.NoError(t, err)
					return
				}
				require.ErrorIs(t, err, ErrInvalidQuery)
			})
		}
	})
}

func TestEnforceQueryMatchersInvalidQuery(t *testing.T) {
	values := url.Values{queryParam: []string{`{app="unclosed`}}
	err := enforceQueryMatchers(values, []*labels.Matcher{requiredTenantMatcher()}, true)
	require.ErrorIs(t, err, ErrInvalidQuery)
}

func TestPartitionRequiredMatchers(t *testing.T) {
	t.Parallel()

	selector, pipeline := partitionRequiredMatchers([]*labels.Matcher{
		requiredTenantMatcher(),
		requiredOrganizationMatcher(),
		requiredUserMatcher(),
	})
	require.Equal(t, []*labels.Matcher{requiredTenantMatcher()}, selector)
	require.Equal(
		t,
		[]*labels.Matcher{requiredOrganizationMatcher(), requiredUserMatcher()},
		pipeline,
	)
}

func TestCombineLabelMatchersPreservesOnlySelectorRequirements(t *testing.T) {
	t.Parallel()

	combined := combineLabelMatchers(
		[]*labels.Matcher{{Type: labels.MatchEqual, Name: "app", Value: "api"}},
		[]*labels.Matcher{requiredTenantMatcher()},
	)
	require.ElementsMatch(t, []string{`app="api"`, `tenant_name="tenant-a"`}, matcherStrings(combined))
}

func matcherStrings(matchers []*labels.Matcher) []string {
	result := make([]string, 0, len(matchers))
	for _, matcher := range matchers {
		result = append(result, matcher.String())
	}
	return result
}
