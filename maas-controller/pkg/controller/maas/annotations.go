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

import metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

// ManagedByODHOperator is used to denote if a resource/component should be reconciled - when missing or true, reconcile.
const ManagedByODHOperator = "opendatahub.io/managed"

// ManagedByMaasODHOperator is used to denote if a resource/component should be reconciled - when missing or true, reconcile.
//
// Deprecated: Exists to preserve backwards compatibility and should not be used.
const ManagedByMaasODHOperator = "maas.opendatahub.io/managed"

// isOptedOut reports whether obj has explicitly opted out of controller management.
// An object is opted out when either the current or the deprecated managed annotation is set to "false".
func isOptedOut(obj metav1.Object) bool {
	annotations := obj.GetAnnotations()
	return annotations[ManagedByODHOperator] == "false" || annotations[ManagedByMaasODHOperator] == "false"
}
