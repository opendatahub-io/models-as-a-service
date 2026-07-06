package modelnaming

import (
	"strings"
	"testing"
)

func TestExternalModelResourceName(t *testing.T) {
	got := ExternalModelResourceName("e2e-external-model")
	if got != "maas-e2e-external-model" {
		t.Fatalf("ExternalModelResourceName() = %q, want %q", got, "maas-e2e-external-model")
	}
}

func TestExternalModelResourceNameTruncatesLongNames(t *testing.T) {
	modelName := strings.Repeat("a", kubernetesNameMaxLength)

	got := ExternalModelResourceName(modelName)

	if len(got) > kubernetesNameMaxLength {
		t.Fatalf("ExternalModelResourceName() length = %d, want <= %d", len(got), kubernetesNameMaxLength)
	}
	if !strings.HasPrefix(got, externalModelResourcePrefix) {
		t.Fatalf("ExternalModelResourceName() = %q, want prefix %q", got, externalModelResourcePrefix)
	}
}
