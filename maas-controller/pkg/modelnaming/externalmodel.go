package modelnaming

import (
	"crypto/sha256"
	"encoding/hex"
	"strings"
)

const (
	externalModelResourcePrefix = "maas-"
	kubernetesNameMaxLength     = 253
	hashLength                  = 8
)

// ExternalModelResourceName returns the name MaaS uses for child resources
// generated from an ExternalModel. The prefix avoids collisions with the
// upstream inference ExternalModel controller, which uses the model name
// directly for its networking resources.
func ExternalModelResourceName(modelName string) string {
	name := externalModelResourcePrefix + modelName
	if len(name) <= kubernetesNameMaxLength {
		return name
	}

	sum := sha256.Sum256([]byte(modelName))
	hash := hex.EncodeToString(sum[:])[:hashLength]
	budget := kubernetesNameMaxLength - len(externalModelResourcePrefix) - len(hash) - 1
	trimmed := strings.Trim(modelName[:budget], "-.")
	if trimmed == "" {
		trimmed = hash
	}
	return externalModelResourcePrefix + trimmed + "-" + hash
}
