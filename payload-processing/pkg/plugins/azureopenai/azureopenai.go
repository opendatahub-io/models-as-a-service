/*
Copyright 2026 The opendatahub.io Authors.

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

package azureopenai

import (
	"fmt"
	"regexp"
)

const (
	// defaultAPIVersion is the default Azure OpenAI API version.
	// Reference: https://learn.microsoft.com/en-us/azure/ai-services/openai/reference
	defaultAPIVersion = "2024-10-21"

	// Azure OpenAI endpoint path template.
	// The deployment ID typically matches the deployed model name.
	// Reference: https://learn.microsoft.com/en-us/azure/ai-foundry/openai/reference#chat-completions
	azurePathTemplate = "/openai/deployments/%s/chat/completions?api-version=%s"
)

// compile-time interface check
// var _ translator.Translator = &AzureOpenAITranslator{}

func NewAzureOpenAITranslator() *AzureOpenAITranslator {
	return &AzureOpenAITranslator{
		apiVersion:          defaultAPIVersion,
		deploymentIDPattern: regexp.MustCompile(`^[a-zA-Z0-9][a-zA-Z0-9._-]*$`), // deploymentIDPattern validates Azure deployment IDs to prevent path/query injection.
	}
}

// AzureOpenAITranslator translates between OpenAI Chat Completions format and
// Azure OpenAI Service format. Azure OpenAI uses the same request/response schema
// as OpenAI, so translation is limited to path rewriting and header adjustments.
type AzureOpenAITranslator struct {
	apiVersion          string
	deploymentIDPattern *regexp.Regexp
}

// TranslateRequest rewrites the path and headers for Azure OpenAI.
// The request body is not mutated since Azure OpenAI accepts the same schema as OpenAI.
// Azure ignores the model field in the body and uses the deployment ID from the URI path.
func (t *AzureOpenAITranslator) TranslateRequest(body map[string]any) (map[string]any, map[string]string, []string, error) {
	model, _ := body["model"].(string)
	if model == "" {
		return nil, nil, nil, fmt.Errorf("model field is required")
	}
	if !t.deploymentIDPattern.MatchString(model) {
		return nil, nil, nil, fmt.Errorf("model contains invalid characters for Azure deployment ID")
	}

	headers := map[string]string{
		":path":        fmt.Sprintf(azurePathTemplate, model, t.apiVersion),
		"content-type": "application/json",
	}

	// Return nil body — no body mutation is needed, Azure accepts the OpenAI request format as-is.
	return nil, headers, nil, nil
}

// TranslateResponse is a no-op since Azure OpenAI returns responses in OpenAI format.
func (t *AzureOpenAITranslator) TranslateResponse(body map[string]any, model string) (map[string]any, error) {
	return nil, nil
}
