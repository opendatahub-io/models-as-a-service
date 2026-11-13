/*
Copyright 2026.

Licensed under the Apache License, Version 2.0 (the "License");
*/

package maas

import (
	"fmt"
	"sort"
	"strings"

	"github.com/opendatahub-io/models-as-a-service/maas-controller/pkg/inferencepaths"
)

const (
	unlimitedRequestLimitName = "requests-unlimited-rerank"
	requestLimitPathPredicate = `request.path.endsWith("` + inferencepaths.RequestCountPathSuffix + `")`
)

// buildRequestCountGroupedLimits mirrors buildGroupedLimits for paths that do
// not return usage.total_tokens. Subscriptions reuse tokenRateLimits numbers
// as interim request budgets on /v1/rerank (see quota docs).
func buildRequestCountGroupedLimits(subs []subInfo) (map[string]any, []string) {
	byGroup := map[string][]subInfo{}
	var groupKeys []string
	seenSub := map[string]struct{}{}
	var subNames []string
	var unlimited []subInfo
	for _, si := range subs {
		name := qualifiedName(si.subNamespace, si.subName)
		if _, ok := seenSub[name]; !ok {
			seenSub[name] = struct{}{}
			subNames = append(subNames, name)
		}
		if si.unlimited {
			unlimited = append(unlimited, si)
			continue
		}
		if _, ok := byGroup[si.groupKey]; !ok {
			groupKeys = append(groupKeys, si.groupKey)
		}
		byGroup[si.groupKey] = append(byGroup[si.groupKey], si)
	}
	sort.Strings(groupKeys)
	sort.Strings(subNames)

	limitsMap := make(map[string]any, len(groupKeys)+1)
	for _, key := range groupKeys {
		addRequestRateGroupLimits(limitsMap, key, byGroup[key])
	}
	if len(unlimited) > 0 {
		limitsMap[unlimitedRequestLimitName] = unlimitedRequestLimit(unlimited)
	}
	return limitsMap, subNames
}

func addRequestRateGroupLimits(limitsMap map[string]any, key string, members []subInfo) {
	var short, legacy []subInfo
	for _, m := range members {
		if m.legacy {
			legacy = append(legacy, m)
			continue
		}
		short = append(short, m)
	}
	name := requestGroupLimitName(key)
	switch {
	case len(short) > 0 && len(legacy) > 0:
		limitsMap[name] = buildRequestGroupLimit(legacy)
		limitsMap[name+"-id"] = buildRequestGroupLimit(short)
	case len(legacy) > 0:
		limitsMap[name] = buildRequestGroupLimit(legacy)
	case len(short) > 0:
		limitsMap[name] = buildRequestGroupLimit(short)
	}
}

func buildRequestGroupLimit(members []subInfo) map[string]any {
	clauses := make([]string, len(members))
	for i, si := range members {
		clauses[i] = subscriptionSelectClause(si)
	}
	sort.Strings(clauses)

	predicate := clauses[0]
	if len(clauses) > 1 {
		predicate = "(" + strings.Join(clauses, " || ") + ")"
	}
	predicate = fmt.Sprintf(`(%s) && %s && !request.path.endsWith("/v1/models")`, predicate, requestLimitPathPredicate)

	return map[string]any{
		"rates": canonicalRates(members[0].rates),
		"when": []any{
			map[string]any{"predicate": predicate},
		},
		"counters": []any{
			map[string]any{"expression": subscriptionCounterExpression(members[0].legacy)},
			map[string]any{"expression": "auth.identity.userid"},
		},
	}
}

func unlimitedRequestLimit(members []subInfo) map[string]any {
	matches := make([]string, 0, len(members))
	for _, si := range members {
		matches = append(matches, subscriptionSelectClause(si))
	}
	sort.Strings(matches)
	return map[string]any{
		"when": []any{
			map[string]any{
				"predicate": fmt.Sprintf(`(%s) && %s && !request.path.endsWith("/v1/models")`, strings.Join(matches, " || "), requestLimitPathPredicate),
			},
		},
	}
}

func requestGroupLimitName(key string) string {
	return "requests-" + strings.NewReplacer("/", "-per-", ",", "-").Replace(key) + "-rerank"
}
