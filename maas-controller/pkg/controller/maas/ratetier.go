/*
Copyright 2026.

Licensed under the Apache License, Version 2.0 (the "License");
you may not use this file except in compliance with the License.
You may obtain a copy of the License at

    http://www.apache.org/licenses/LICENSE-2.0

Unless required by applicable law or agreed to in writing, software
distributed under the License is distributed on an "AS IS" BASIS,
WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
See the License for the specific language governing permissions and
limitations under the License.
*/

package maas

import (
	"fmt"
	"sort"
	"strconv"
	"strings"
)

// subInfo is one (subscription, modelRef) pair that reconcileTRLPForModel
// needs to place in the grouped TokenRateLimitPolicy: the rates that put it in
// a group (groupKey), or unlimited when it has no token budget, the fully
// model-scoped identity that goes into that group's predicate (modelScoped),
// and the subscription's own name for the TRLP's tracking annotation.
type subInfo struct {
	subNamespace, subName string
	rates                 []any
	unlimited             bool
	modelScoped           string
	groupKey              string
}

// buildGroupedLimits turns the valid (subscription, modelRef) pairs for one
// model into a TokenRateLimitPolicy limits map: one limit per distinct rate
// set, predicate the OR of that group's subscriptions' selected_subscription_key,
// counters keyed on both selected_subscription_key and userid so subscriptions
// sharing a limit still get independent budgets. It also returns the sorted,
// deduplicated subscription names for the TRLP's tracking annotation.
// Unlimited subscriptions go into the shared unlimitedLimitName limit instead
// of a rate group, and are listed in the annotation too: cleanupStaleTRLPs
// relies on it to rebuild the TRLP when a subscription drops the model.
//
// Output is fully deterministic (sorted group keys, sorted member refs) so an
// unchanged input reconciles to a byte-identical spec and never triggers a
// spurious update.
func buildGroupedLimits(subs []subInfo) (map[string]any, []string) {
	byGroup := map[string][]subInfo{}
	var groupKeys []string
	seenSub := map[string]struct{}{}
	var subNames []string
	var unlimitedKeys []string
	for _, si := range subs {
		name := qualifiedName(si.subNamespace, si.subName)
		if _, ok := seenSub[name]; !ok {
			seenSub[name] = struct{}{}
			subNames = append(subNames, name)
		}

		if si.unlimited {
			unlimitedKeys = append(unlimitedKeys, si.modelScoped)
			continue
		}
		if _, ok := byGroup[si.groupKey]; !ok {
			groupKeys = append(groupKeys, si.groupKey)
		}
		byGroup[si.groupKey] = append(byGroup[si.groupKey], si)
	}
	sort.Strings(groupKeys)
	sort.Strings(subNames)

	limitsMap := make(map[string]any, len(groupKeys))
	for _, key := range groupKeys {
		limitsMap[rateGroupLimitName(key)] = buildGroupLimit(byGroup[key])
	}
	if len(unlimitedKeys) > 0 {
		limitsMap[unlimitedLimitName] = unlimitedTokenLimit(unlimitedKeys)
	}
	return limitsMap, subNames
}

// buildGroupLimit renders one rate group's members into a TokenRateLimitPolicy
// limit. Members share a rate set but may list it in different orders or with
// duplicates, and List gives no ordering guarantee, so the rates are rendered
// in canonical order rather than as any one member wrote them - otherwise the
// spec could change between reconciles and trigger needless policy updates.
func buildGroupLimit(members []subInfo) map[string]any {
	refs := make([]string, len(members))
	for i, si := range members {
		refs[i] = si.modelScoped
	}
	sort.Strings(refs)

	clauses := make([]string, len(refs))
	for i, ref := range refs {
		clauses[i] = fmt.Sprintf(`auth.identity.selected_subscription_key == "%s"`, ref)
	}
	predicate := clauses[0]
	if len(clauses) > 1 {
		predicate = "(" + strings.Join(clauses, " || ") + ")"
	}

	return map[string]any{
		"rates": canonicalRates(members[0].rates),
		"when": []any{
			map[string]any{
				// Exempt /v1/models endpoint from token rate limiting.
				// This endpoint is used for model discovery/metadata and does not consume inference tokens.
				// Users should be able to query model capabilities even when their token quota is exhausted.
				"predicate": fmt.Sprintf(`%s && !request.path.endsWith("/v1/models")`, predicate),
			},
		},
		"counters": []any{
			map[string]any{"expression": "auth.identity.selected_subscription_key"},
			map[string]any{"expression": "auth.identity.userid"},
		},
	}
}

// rateGroupKey renders a subscription's token rate limits into a string that
// is identical for every subscription sharing the same rates, so
// reconcileTRLPForModel can group them into one TokenRateLimitPolicy limit
// instead of one per subscription (RHOAIENG-95277: Kuadrant copies every
// limit into every ActionSet of the route, so the object's size scales with
// the number of limits, not with the number of subscriptions behind them).
//
// It is used only as an internal map key inside this package - never
// serialized, never sent to maas-api, never exposed on an identity or a CR.
// Rates are rendered "<limit>/<window>", deduped and sorted, so windows are
// kept verbatim: "1m" and "60s" are different groups.
func rateGroupKey(rates []any) string {
	key, _ := canonicalizeRates(rates)
	return key
}

// canonicalRates returns rates deduped and sorted in rateGroupKey order, so
// every member of a group renders the same rates list.
func canonicalRates(rates []any) []any {
	_, canonical := canonicalizeRates(rates)
	return canonical
}

// canonicalizeRates computes the group key and the canonical rates list in one
// pass so the two can never disagree.
func canonicalizeRates(rates []any) (string, []any) {
	byKey := make(map[string]any, len(rates))
	keys := make([]string, 0, len(rates))
	for _, r := range rates {
		m, ok := r.(map[string]any)
		if !ok {
			continue
		}
		limit, _ := m["limit"].(int64)
		window, _ := m["window"].(string)
		key := strconv.FormatInt(limit, 10) + "/" + window
		if _, dup := byKey[key]; dup {
			continue
		}
		byKey[key] = r
		keys = append(keys, key)
	}
	sort.Strings(keys)
	canonical := make([]any, len(keys))
	for i, k := range keys {
		canonical[i] = byKey[k]
	}
	return strings.Join(keys, ","), canonical
}

// rateGroupLimitName turns a rateGroupKey into a TokenRateLimitPolicy limit
// key, which must not contain slashes. validateTokenRateLimit never lets a
// limit or window contain "-" or ",", so two distinct groups can't collide.
func rateGroupLimitName(key string) string {
	return "tokens-" + strings.NewReplacer("/", "-per-", ",", "-").Replace(key)
}
