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

package azureopenai

import (
	"fmt"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestTranslateRequest_BasicChat(t *testing.T) {
	body := map[string]any{
		"model": "gpt-4o",
		"messages": []any{
			map[string]any{"role": "user", "content": "What is 2+2?"},
		},
	}

	translatedBody, headers, headersToRemove, err := NewAzureOpenAITranslator().TranslateRequest(body)
	require.NoError(t, err)

	assert.Nil(t, translatedBody, "body should not be mutated for Azure OpenAI")

	expectedPath := fmt.Sprintf("/openai/deployments/gpt-4o/chat/completions?api-version=%s", defaultAPIVersion)
	assert.Equal(t, expectedPath, headers[":path"])
	assert.Equal(t, "application/json", headers["content-type"])

	assert.Empty(t, headersToRemove)
}

func TestTranslateRequest_ModelUsedAsDeploymentID(t *testing.T) {
	tests := []struct {
		name  string
		model string
	}{
		{"gpt-4o model", "gpt-4o"},
		{"gpt-4o-mini model", "gpt-4o-mini"},
		{"custom deployment name", "my-custom-deployment"},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			body := map[string]any{
				"model":    tt.model,
				"messages": []any{map[string]any{"role": "user", "content": "Hi"}},
			}

			_, headers, _, err := NewAzureOpenAITranslator().TranslateRequest(body)
			require.NoError(t, err)

			expectedPath := fmt.Sprintf("/openai/deployments/%s/chat/completions?api-version=%s", tt.model, defaultAPIVersion)
			assert.Equal(t, expectedPath, headers[":path"])
		})
	}
}

func TestTranslateRequest_MissingModel(t *testing.T) {
	body := map[string]any{
		"messages": []any{map[string]any{"role": "user", "content": "Hi"}},
	}

	_, _, _, err := NewAzureOpenAITranslator().TranslateRequest(body)
	assert.Error(t, err)
	assert.Contains(t, err.Error(), "model")
}

func TestTranslateRequest_EmptyModel(t *testing.T) {
	body := map[string]any{
		"model":    "",
		"messages": []any{map[string]any{"role": "user", "content": "Hi"}},
	}

	_, _, _, err := NewAzureOpenAITranslator().TranslateRequest(body)
	assert.Error(t, err)
	assert.Contains(t, err.Error(), "model")
}

func TestTranslateRequest_InvalidModelCharacters(t *testing.T) {
	tests := []struct {
		name  string
		model string
	}{
		{"query injection", "gpt-4o?api-version=hijacked&x="},
		{"path traversal", "../../../etc/passwd"},
		{"slash in model", "org/model-name"},
		{"space in model", "gpt 4o"},
		{"starts with hyphen", "-gpt-4o"},
		{"starts with dot", ".hidden"},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			body := map[string]any{
				"model":    tt.model,
				"messages": []any{map[string]any{"role": "user", "content": "Hi"}},
			}

			_, _, _, err := NewAzureOpenAITranslator().TranslateRequest(body)
			assert.Error(t, err)
			assert.Contains(t, err.Error(), "invalid characters")
		})
	}
}

func TestTranslateRequest_ValidModelCharacters(t *testing.T) {
	tests := []struct {
		name  string
		model string
	}{
		{"simple model", "gpt-4o"},
		{"with dots", "gpt-4o.2025"},
		{"with underscore", "my_deployment"},
		{"alphanumeric", "model123"},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			body := map[string]any{
				"model":    tt.model,
				"messages": []any{map[string]any{"role": "user", "content": "Hi"}},
			}

			_, _, _, err := NewAzureOpenAITranslator().TranslateRequest(body)
			assert.NoError(t, err)
		})
	}
}

func TestTranslateRequest_defaultAPIVersion(t *testing.T) {
	p := NewAzureOpenAITranslator()
	assert.Equal(t, defaultAPIVersion, p.apiVersion)
}

func TestTranslateRequest_BodyPassthrough(t *testing.T) {
	body := map[string]any{
		"model": "gpt-4o",
		"messages": []any{
			map[string]any{"role": "system", "content": "You are helpful."},
			map[string]any{"role": "user", "content": "Hello"},
		},
		"temperature":       0.7,
		"top_p":             0.9,
		"max_tokens":        float64(1000),
		"stream":            true,
		"stop":              []any{"END"},
		"n":                 float64(1),
		"presence_penalty":  0.5,
		"frequency_penalty": 0.3,
	}

	translatedBody, _, _, err := NewAzureOpenAITranslator().TranslateRequest(body)
	require.NoError(t, err)

	assert.Nil(t, translatedBody, "Azure OpenAI should not mutate the request body")
}

func TestTranslateRequest_HeadersSet(t *testing.T) {
	body := map[string]any{
		"model":    "gpt-4o",
		"messages": []any{map[string]any{"role": "user", "content": "Hi"}},
	}

	_, headers, _, err := NewAzureOpenAITranslator().TranslateRequest(body)
	require.NoError(t, err)

	assert.Len(t, headers, 2)
	assert.Contains(t, headers, ":path")
	assert.Contains(t, headers, "content-type")
}

func TestTranslateRequest_HeadersRemoved(t *testing.T) {
	body := map[string]any{
		"model":    "gpt-4o",
		"messages": []any{map[string]any{"role": "user", "content": "Hi"}},
	}

	_, _, headersToRemove, err := NewAzureOpenAITranslator().TranslateRequest(body)
	require.NoError(t, err)

	assert.Empty(t, headersToRemove)
}

func TestTranslateResponse_Passthrough(t *testing.T) {
	body := map[string]any{
		"id":      "chatcmpl-abc123",
		"object":  "chat.completion",
		"created": float64(1700000000),
		"model":   "gpt-4o",
		"choices": []any{
			map[string]any{
				"index": float64(0),
				"message": map[string]any{
					"role":    "assistant",
					"content": "The answer is 4.",
				},
				"finish_reason": "stop",
			},
		},
		"usage": map[string]any{
			"prompt_tokens":     float64(10),
			"completion_tokens": float64(5),
			"total_tokens":      float64(15),
		},
	}

	translatedBody, err := NewAzureOpenAITranslator().TranslateResponse(body, "gpt-4o")
	require.NoError(t, err)
	assert.Nil(t, translatedBody, "Azure OpenAI response should not be mutated — already in OpenAI format")
}

func TestTranslateResponse_EmptyBody(t *testing.T) {
	body := map[string]any{}

	translatedBody, err := NewAzureOpenAITranslator().TranslateResponse(body, "gpt-4o")
	require.NoError(t, err)
	assert.Nil(t, translatedBody)
}

func TestTranslateResponse_ErrorPassthrough(t *testing.T) {
	body := map[string]any{
		"error": map[string]any{
			"message": "The API deployment for this resource does not exist.",
			"type":    "invalid_request_error",
			"code":    "DeploymentNotFound",
		},
	}

	translatedBody, err := NewAzureOpenAITranslator().TranslateResponse(body, "gpt-4o")
	require.NoError(t, err)
	assert.Nil(t, translatedBody, "Azure error responses are already in OpenAI format")
}

func TestTranslateResponse_StreamingChunkPassthrough(t *testing.T) {
	body := map[string]any{
		"id":      "chatcmpl-abc123",
		"object":  "chat.completion.chunk",
		"created": float64(1700000000),
		"model":   "gpt-4o",
		"choices": []any{
			map[string]any{
				"index": float64(0),
				"delta": map[string]any{
					"content": "Hello",
				},
				"finish_reason": nil,
			},
		},
	}

	translatedBody, err := NewAzureOpenAITranslator().TranslateResponse(body, "gpt-4o")
	require.NoError(t, err)
	assert.Nil(t, translatedBody)
}
