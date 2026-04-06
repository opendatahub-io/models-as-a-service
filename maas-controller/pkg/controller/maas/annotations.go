/*
Copyright 2025.

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
	"regexp"
	"strings"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

// ManagedByODHOperator is used to denote if a resource/component should be reconciled - when missing or true, reconcile.
const ManagedByODHOperator = "opendatahub.io/managed"

// validVirtualNamePattern validates virtual model names (metadata.name and aliases) to prevent injection attacks.
// Allows alphanumeric, hyphens, underscores, dots. Must start with alphanumeric.
var validVirtualNamePattern = regexp.MustCompile(`^[a-zA-Z0-9][a-zA-Z0-9._-]*$`)

// validBackendModelPattern validates backend model names to ensure compatibility.
// Allows alphanumeric, hyphens, underscores, dots. Must start with alphanumeric.
var validBackendModelPattern = regexp.MustCompile(`^[a-zA-Z0-9][a-zA-Z0-9._-]*$`)

// isManaged reports whether obj has explicitly opted out of maas or opendatahub controller management.
func isManaged(obj metav1.Object) bool {
	annotations := obj.GetAnnotations()
	val, ok := annotations[ManagedByODHOperator]

	if !ok {
		// Annotation is absent -> is managed
		return true
	}

	return val != "false"
}

// validateVirtualName validates a virtual model name (resource name or alias).
// Returns true if valid, false otherwise.
func validateVirtualName(name string) bool {
	if name == "" {
		return false
	}
	return validVirtualNamePattern.MatchString(name)
}

// validateBackendModelName validates a backend model name.
// Returns true if valid, false otherwise.
func validateBackendModelName(name string) bool {
	if name == "" {
		return false
	}
	return validBackendModelPattern.MatchString(name)
}

// validateModelAliases validates a slice of model aliases.
// Returns a slice of valid aliases, skipping invalid ones.
func validateModelAliases(aliases []string) []string {
	var validAliases []string
	seen := make(map[string]bool)

	for _, alias := range aliases {
		alias = strings.TrimSpace(alias)
		if alias == "" {
			continue
		}

		// Skip duplicates
		if seen[alias] {
			continue
		}

		if validateVirtualName(alias) {
			validAliases = append(validAliases, alias)
			seen[alias] = true
		}
		// Invalid aliases are silently skipped
	}

	return validAliases
}

// getVirtualNames returns all virtual names for a resource (metadata.name + modelAliases).
// This is used to populate status.virtualNames during reconciliation.
func getVirtualNames(resourceName string, modelAliases []string) []string {
	virtualNames := []string{resourceName}
	
	// Validate and add aliases
	validAliases := validateModelAliases(modelAliases)
	virtualNames = append(virtualNames, validAliases...)
	
	return virtualNames
}

// getBackendModelName returns the resolved backend model name.
// Uses spec.backendModelName if specified, otherwise falls back to fallbackName.
func getBackendModelName(specBackendModelName, fallbackName string) string {
	if specBackendModelName != "" && validateBackendModelName(specBackendModelName) {
		return specBackendModelName
	}
	return fallbackName
}

// equalStringSlices compares two string slices for equality
func equalStringSlices(a, b []string) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i] != b[i] {
			return false
		}
	}
	return true
}
