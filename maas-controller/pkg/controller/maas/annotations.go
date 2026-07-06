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
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	"github.com/opendatahub-io/models-as-a-service/maas-controller/pkg/platform/tenantreconcile"
)

// ManagedByODHOperator is used to denote if a resource/component should be reconciled - when missing or true, reconcile.
// This aliases tenantreconcile.AnnotationManaged so there is a single canonical definition.
const ManagedByODHOperator = tenantreconcile.AnnotationManaged

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
