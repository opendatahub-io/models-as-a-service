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
	"cmp"
	"errors"
	"fmt"
	"slices"
	"strconv"
)

// ErrInvalidWindow is returned when a rate-limit window cannot be parsed.
var ErrInvalidWindow = errors.New("invalid rate-limit window")

// Generates entries for the shared Limitador CR's spec.limits. The token-usage
// descriptor key must match the pluginConfig action set for the counter to line up.

// LimitadorLimit is one entry in the Limitador CR's spec.limits.
type LimitadorLimit struct {
	Name       string   `json:"name"`
	Namespace  string   `json:"namespace"`
	MaxValue   int64    `json:"max_value"`
	Seconds    int64    `json:"seconds"`
	Conditions []string `json:"conditions"`
	Variables  []string `json:"variables"`
}

// userIDCounter is the per-user counter variable every MaaS limit is keyed on.
const userIDCounter = `descriptors[0]["auth.identity.userid"]`

// buildLimitadorLimit compiles one token-rate limit into a Limitador entry.
func buildLimitadorLimit(name, namespace, tokenLimitKey string, maxValue int64, window string) (LimitadorLimit, error) {
	seconds, err := windowSeconds(window)
	if err != nil {
		return LimitadorLimit{}, err
	}
	return LimitadorLimit{
		Name:       name,
		Namespace:  namespace,
		MaxValue:   maxValue,
		Seconds:    seconds,
		Conditions: []string{fmt.Sprintf(`descriptors[0]["tokenlimit.%s"] == "1"`, tokenLimitKey)},
		Variables:  []string{userIDCounter},
	}, nil
}

// mergeLimits replaces this gateway's limits in the shared Limitador CR without
// disturbing other tenants'. ownedScopes is the set of RLS domains (limit
// Namespace) this gateway owns; existing limits in those scopes are dropped and
// replaced by ours. Sorted for a stable CR.
func mergeLimits(existing []LimitadorLimit, ownedScopes map[string]bool, ours []LimitadorLimit) []LimitadorLimit {
	out := make([]LimitadorLimit, 0, len(existing)+len(ours))
	for _, l := range existing {
		if !ownedScopes[l.Namespace] {
			out = append(out, l)
		}
	}
	out = append(out, ours...)
	slices.SortFunc(out, func(a, b LimitadorLimit) int {
		return cmp.Or(cmp.Compare(a.Namespace, b.Namespace), cmp.Compare(a.Name, b.Name))
	})
	return out
}

// windowSeconds converts a TRLP window ("<n>s|m|h") to seconds.
func windowSeconds(window string) (int64, error) {
	if len(window) < 2 {
		return 0, fmt.Errorf("%w: %q", ErrInvalidWindow, window)
	}
	unit := window[len(window)-1]
	n, err := strconv.ParseInt(window[:len(window)-1], 10, 64)
	if err != nil {
		return 0, fmt.Errorf("%w: %q: %w", ErrInvalidWindow, window, err)
	}
	switch unit {
	case 's':
		return n, nil
	case 'm':
		return n * 60, nil
	case 'h':
		return n * 3600, nil
	default:
		return 0, fmt.Errorf("%w: %q (want s, m, or h)", ErrInvalidWindow, window)
	}
}
