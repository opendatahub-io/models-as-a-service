package models

import (
	"encoding/json"
	"fmt"
	"reflect"
	"strings"

	"github.com/openai/openai-go/v2"
	"knative.dev/pkg/apis"
)

// Details contains additional metadata from LLMInferenceService annotations.
type Details struct {
	GenAIUseCase string `json:"genaiUseCase,omitempty"`
	Description  string `json:"description,omitempty"`
	DisplayName  string `json:"displayName,omitempty"`
}

// Model extends openai.Model with additional fields.
//
// The ID field contains the canonical model identifier, which is used for metrics,
// billing, and API responses. When a model server returns multiple model names
// (e.g., vLLM served model names), the first discovered model becomes the canonical ID.
// Alternative model names are stored in the Aliases field.
type Model struct {
	openai.Model `json:",inline"`

	// Kind is the model reference kind (e.g. "llmisvc" from MaaSModel spec.modelRef.kind).
	// Used when validating access; default is "llmisvc" if unset.
	Kind    string    `json:"kind,omitempty"`
	URL     *apis.URL `json:"url,omitempty"`
	Ready   bool      `json:"ready"`
	Details *Details  `json:"modelDetails,omitempty"`
	Aliases []string  `json:"aliases,omitempty"`
}

// UnmarshalJSON implements custom JSON unmarshalling to work around openai.Model's
// custom unmarshalling that captures all unknown fields.
func (m *Model) UnmarshalJSON(data []byte) error {
	if err := m.Model.UnmarshalJSON(data); err != nil {
		return err
	}

	return m.extractFieldsFromExtraFields()
}

// extractFieldsFromExtraFields uses reflection to automatically populate all
// additional fields (beyond openai.Model) from the ExtraFields map.
func (m *Model) extractFieldsFromExtraFields() error {
	modelValue := reflect.ValueOf(m).Elem()
	modelType := modelValue.Type()

	for i := range modelType.NumField() {
		field := modelType.Field(i)
		fieldValue := modelValue.Field(i)

		// Skip the embedded openai.Model field and unexported fields
		if field.Name == "Model" || !fieldValue.CanSet() {
			continue
		}

		jsonTag := field.Tag.Get("json")
		if jsonTag == "" || jsonTag == "-" {
			continue
		}

		jsonFieldName := strings.Split(jsonTag, ",")[0]
		if jsonFieldName == "" {
			jsonFieldName = strings.ToLower(field.Name)
		}

		if extraField, exists := m.JSON.ExtraFields[jsonFieldName]; exists {
			if err := m.setFieldFromExtraField(fieldValue, field.Type, extraField); err != nil {
				return fmt.Errorf("failed setting %s: %w", jsonFieldName, err)
			}
		}
	}

	return nil
}

func (m *Model) setFieldFromExtraField(fieldValue reflect.Value, fieldType reflect.Type, extraField any) error {
	rawValue := ""
	if rf, ok := extraField.(interface{ Raw() string }); ok {
		rawValue = rf.Raw()
	}

	newValue := reflect.New(fieldType)
	if err := json.Unmarshal([]byte(rawValue), newValue.Interface()); err != nil {
		return err
	}
	fieldValue.Set(newValue.Elem())

	return nil
}
