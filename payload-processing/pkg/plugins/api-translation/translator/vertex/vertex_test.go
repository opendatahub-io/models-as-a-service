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

package vertex

import (
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestVertexV1BetaPathTemplate(t *testing.T) {
	assert.Equal(t, "/v1beta/models/%s:generateContent", vertexV1BetaPathTemplate)
}

func TestTranslateRequest_BasicChat(t *testing.T) {
	body := map[string]any{
		"model": "gemini-2.5-flash",
		"messages": []any{
			map[string]any{"role": "user", "content": "What is 2+2?"},
		},
	}

	translated, headers, headersToRemove, err := NewVertexTranslator().TranslateRequest(body)
	require.NoError(t, err)

	contents := translated["contents"].([]map[string]any)
	require.Len(t, contents, 1)
	assert.Equal(t, "user", contents[0]["role"])

	parts := contents[0]["parts"].([]map[string]any)
	assert.Equal(t, "What is 2+2?", parts[0]["text"])

	assert.Nil(t, translated["systemInstruction"])
	assert.Nil(t, translated["model"])

	assert.Equal(t, "application/json", headers["content-type"])
	assert.Equal(t, "/v1beta/models/gemini-2.5-flash:generateContent", headers[":path"])

	assert.Nil(t, headersToRemove)
}

func TestTranslateRequest_SystemMessage(t *testing.T) {
	body := map[string]any{
		"model": "gemini-2.5-flash",
		"messages": []any{
			map[string]any{"role": "system", "content": "You are a helpful assistant."},
			map[string]any{"role": "user", "content": "Hello"},
		},
	}

	translated, _, _, err := NewVertexTranslator().TranslateRequest(body)
	require.NoError(t, err)

	sysInstruction := translated["systemInstruction"].(map[string]any)
	sysParts := sysInstruction["parts"].([]map[string]any)
	require.Len(t, sysParts, 1)
	assert.Equal(t, "You are a helpful assistant.", sysParts[0]["text"])

	contents := translated["contents"].([]map[string]any)
	require.Len(t, contents, 1)
	assert.Equal(t, "user", contents[0]["role"])
}

func TestTranslateRequest_MultipleMessages(t *testing.T) {
	body := map[string]any{
		"model": "gemini-2.5-flash",
		"messages": []any{
			map[string]any{"role": "user", "content": "Hi"},
			map[string]any{"role": "assistant", "content": "Hello!"},
			map[string]any{"role": "user", "content": "How are you?"},
		},
	}

	translated, _, _, err := NewVertexTranslator().TranslateRequest(body)
	require.NoError(t, err)

	contents := translated["contents"].([]map[string]any)
	require.Len(t, contents, 3)
	assert.Equal(t, "user", contents[0]["role"])
	assert.Equal(t, "model", contents[1]["role"])
	assert.Equal(t, "user", contents[2]["role"])

	parts1 := contents[1]["parts"].([]map[string]any)
	assert.Equal(t, "Hello!", parts1[0]["text"])
}

func TestTranslateRequest_DeveloperRole(t *testing.T) {
	body := map[string]any{
		"model": "gemini-2.5-flash",
		"messages": []any{
			map[string]any{"role": "developer", "content": "You are a coding assistant."},
			map[string]any{"role": "user", "content": "Hello"},
		},
	}

	translated, _, _, err := NewVertexTranslator().TranslateRequest(body)
	require.NoError(t, err)

	sysInstruction := translated["systemInstruction"].(map[string]any)
	sysParts := sysInstruction["parts"].([]map[string]any)
	require.Len(t, sysParts, 1)
	assert.Equal(t, "You are a coding assistant.", sysParts[0]["text"])
}

func TestTranslateRequest_SystemAndDeveloperConcatenated(t *testing.T) {
	body := map[string]any{
		"model": "gemini-2.5-flash",
		"messages": []any{
			map[string]any{"role": "system", "content": "Be concise."},
			map[string]any{"role": "developer", "content": "Use markdown."},
			map[string]any{"role": "user", "content": "Hello"},
		},
	}

	translated, _, _, err := NewVertexTranslator().TranslateRequest(body)
	require.NoError(t, err)

	sysInstruction := translated["systemInstruction"].(map[string]any)
	sysParts := sysInstruction["parts"].([]map[string]any)
	require.Len(t, sysParts, 2)
	assert.Equal(t, "Be concise.", sysParts[0]["text"])
	assert.Equal(t, "Use markdown.", sysParts[1]["text"])
}

func TestTranslateRequest_OptionalParams(t *testing.T) {
	body := map[string]any{
		"model":       "gemini-2.5-flash",
		"messages":    []any{map[string]any{"role": "user", "content": "Hi"}},
		"temperature": 0.7,
		"top_p":       0.9,
		"stop":        []any{"END", "STOP"},
		"max_tokens":  float64(500),
	}

	translated, _, _, err := NewVertexTranslator().TranslateRequest(body)
	require.NoError(t, err)

	config := translated["generationConfig"].(map[string]any)
	assert.Equal(t, 0.7, config["temperature"])
	assert.Equal(t, 0.9, config["topP"])
	assert.Equal(t, []string{"END", "STOP"}, config["stopSequences"])
	assert.Equal(t, 500, config["maxOutputTokens"])
}

func TestTranslateRequest_MaxTokens(t *testing.T) {
	tests := []struct {
		name     string
		body     map[string]any
		expected int
	}{
		{
			name: "max_completion_tokens takes priority",
			body: map[string]any{
				"model":                 "gemini-2.5-flash",
				"messages":              []any{map[string]any{"role": "user", "content": "Hi"}},
				"max_completion_tokens": float64(200),
				"max_tokens":            float64(100),
			},
			expected: 200,
		},
		{
			name: "max_tokens fallback",
			body: map[string]any{
				"model":      "gemini-2.5-flash",
				"messages":   []any{map[string]any{"role": "user", "content": "Hi"}},
				"max_tokens": float64(500),
			},
			expected: 500,
		},
		{
			name: "no max tokens omits field",
			body: map[string]any{
				"model":    "gemini-2.5-flash",
				"messages": []any{map[string]any{"role": "user", "content": "Hi"}},
			},
			expected: 0,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			translated, _, _, err := NewVertexTranslator().TranslateRequest(tt.body)
			require.NoError(t, err)

			config, hasConfig := translated["generationConfig"].(map[string]any)
			if tt.expected > 0 {
				require.True(t, hasConfig)
				assert.Equal(t, tt.expected, config["maxOutputTokens"])
			} else {
				if hasConfig {
					_, hasMaxTokens := config["maxOutputTokens"]
					assert.False(t, hasMaxTokens)
				}
			}
		})
	}
}

func TestTranslateRequest_StopString(t *testing.T) {
	body := map[string]any{
		"model":    "gemini-2.5-flash",
		"messages": []any{map[string]any{"role": "user", "content": "Hi"}},
		"stop":     "END",
	}

	translated, _, _, err := NewVertexTranslator().TranslateRequest(body)
	require.NoError(t, err)

	config := translated["generationConfig"].(map[string]any)
	assert.Equal(t, []string{"END"}, config["stopSequences"])
}

func TestTranslateRequest_InvalidModelCharacters(t *testing.T) {
	tests := []struct {
		name  string
		model string
	}{
		{"path traversal", "../../admin"},
		{"encoded traversal", "%2e%2e%2fadmin"},
		{"encoded slash", "gemini%2fadmin"},
		{"query injection", "model?key=val"},
		{"crlf injection", "gemini%0d%0aX-Injected:1"},
		{"backslash traversal", `..\\admin`},
		{"null byte", "gemini\x00admin"},
		{"slash in model", "models/gemini"},
		{"space in model", "gemini pro"},
		{"starts with hyphen", "-gemini"},
		{"starts with dot", ".gemini"},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			body := map[string]any{
				"model":    tt.model,
				"messages": []any{map[string]any{"role": "user", "content": "Hi"}},
			}
			_, _, _, err := NewVertexTranslator().TranslateRequest(body)
			assert.Error(t, err)
			assert.Contains(t, err.Error(), "invalid characters")
		})
	}
}

func TestTranslateRequest_ValidModelNames(t *testing.T) {
	tests := []struct {
		name  string
		model string
	}{
		{"simple", "gemini-pro"},
		{"with version", "gemini-2.5-flash"},
		{"with dots", "gemini-2.0-flash-001"},
		{"with underscore", "gemini_pro"},
		{"alphanumeric", "model123"},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			body := map[string]any{
				"model":    tt.model,
				"messages": []any{map[string]any{"role": "user", "content": "Hi"}},
			}
			_, _, _, err := NewVertexTranslator().TranslateRequest(body)
			assert.NoError(t, err)
		})
	}
}

func TestTranslateRequest_MissingModel(t *testing.T) {
	body := map[string]any{
		"messages": []any{map[string]any{"role": "user", "content": "Hi"}},
	}

	_, _, _, err := NewVertexTranslator().TranslateRequest(body)
	assert.Error(t, err)
	assert.Contains(t, err.Error(), "model")
}

func TestTranslateRequest_MissingMessages(t *testing.T) {
	body := map[string]any{
		"model": "gemini-2.5-flash",
	}

	_, _, _, err := NewVertexTranslator().TranslateRequest(body)
	assert.Error(t, err)
	assert.Contains(t, err.Error(), "messages")
}

func TestTranslateRequest_EmptyMessagesArray(t *testing.T) {
	body := map[string]any{
		"model":    "gemini-2.5-flash",
		"messages": []any{},
	}

	_, _, _, err := NewVertexTranslator().TranslateRequest(body)
	assert.Error(t, err)
	assert.Contains(t, err.Error(), "non-system message")
}

func TestTranslateRequest_OnlySystemMessage(t *testing.T) {
	body := map[string]any{
		"model": "gemini-2.5-flash",
		"messages": []any{
			map[string]any{"role": "system", "content": "You are helpful"},
		},
	}

	_, _, _, err := NewVertexTranslator().TranslateRequest(body)
	assert.Error(t, err)
	assert.Contains(t, err.Error(), "non-system message")
}

func TestTranslateRequest_ToolRoleRejected(t *testing.T) {
	body := map[string]any{
		"model": "gemini-2.5-flash",
		"messages": []any{
			map[string]any{"role": "user", "content": "Hi"},
			map[string]any{"role": "tool", "content": "result", "tool_call_id": "abc"},
		},
	}

	_, _, _, err := NewVertexTranslator().TranslateRequest(body)
	assert.Error(t, err)
	assert.Contains(t, err.Error(), "tool")
	assert.Contains(t, err.Error(), "not supported")
}

func TestTranslateRequest_FunctionRoleRejected(t *testing.T) {
	body := map[string]any{
		"model": "gemini-2.5-flash",
		"messages": []any{
			map[string]any{"role": "function", "content": "result"},
		},
	}

	_, _, _, err := NewVertexTranslator().TranslateRequest(body)
	assert.Error(t, err)
	assert.Contains(t, err.Error(), "function")
	assert.Contains(t, err.Error(), "not supported")
}

func TestTranslateRequest_UnknownRoleRejected(t *testing.T) {
	body := map[string]any{
		"model": "gemini-2.5-flash",
		"messages": []any{
			map[string]any{"role": "narrator", "content": "Once upon a time"},
		},
	}

	_, _, _, err := NewVertexTranslator().TranslateRequest(body)
	assert.Error(t, err)
	assert.Contains(t, err.Error(), "unknown role")
}

func TestTranslateRequest_ContentParts(t *testing.T) {
	body := map[string]any{
		"model": "gemini-2.5-flash",
		"messages": []any{
			map[string]any{
				"role": "user",
				"content": []any{
					map[string]any{"type": "text", "text": "Hello"},
					map[string]any{"type": "text", "text": "World"},
				},
			},
		},
	}

	translated, _, _, err := NewVertexTranslator().TranslateRequest(body)
	require.NoError(t, err)

	contents := translated["contents"].([]map[string]any)
	parts := contents[0]["parts"].([]map[string]any)
	assert.Equal(t, "Hello World", parts[0]["text"])
}

func TestTranslateRequest_EmptyContentString(t *testing.T) {
	body := map[string]any{
		"model": "gemini-2.5-flash",
		"messages": []any{
			map[string]any{"role": "user", "content": ""},
		},
	}

	translated, _, _, err := NewVertexTranslator().TranslateRequest(body)
	require.NoError(t, err)

	contents := translated["contents"].([]map[string]any)
	parts := contents[0]["parts"].([]map[string]any)
	assert.Equal(t, "", parts[0]["text"])
}

func TestTranslateRequest_PathIncludesModel(t *testing.T) {
	body := map[string]any{
		"model":    "gemini-2.0-flash-001",
		"messages": []any{map[string]any{"role": "user", "content": "Hi"}},
	}

	_, headers, _, err := NewVertexTranslator().TranslateRequest(body)
	require.NoError(t, err)
	assert.Equal(t, "/v1beta/models/gemini-2.0-flash-001:generateContent", headers[":path"])
}

func TestTranslateRequest_NoGenerationConfigWhenNoParams(t *testing.T) {
	body := map[string]any{
		"model":    "gemini-2.5-flash",
		"messages": []any{map[string]any{"role": "user", "content": "Hi"}},
	}

	translated, _, _, err := NewVertexTranslator().TranslateRequest(body)
	require.NoError(t, err)

	_, hasConfig := translated["generationConfig"]
	assert.False(t, hasConfig)
}

func TestTranslateRequest_NonTextContentSkipped(t *testing.T) {
	body := map[string]any{
		"model": "gemini-2.5-flash",
		"messages": []any{
			map[string]any{
				"role": "user",
				"content": []any{
					map[string]any{"type": "text", "text": "Describe this"},
					map[string]any{"type": "image_url", "image_url": map[string]any{"url": "https://example.com/img.png"}},
				},
			},
		},
	}

	translated, _, _, err := NewVertexTranslator().TranslateRequest(body)
	require.NoError(t, err)

	contents := translated["contents"].([]map[string]any)
	parts := contents[0]["parts"].([]map[string]any)
	assert.Equal(t, "Describe this", parts[0]["text"])
}

func TestTranslateRequest_MessagesNotArray(t *testing.T) {
	body := map[string]any{
		"model":    "gemini-2.5-flash",
		"messages": "not-an-array",
	}
	_, _, _, err := NewVertexTranslator().TranslateRequest(body)
	assert.Error(t, err)
	assert.Contains(t, err.Error(), "messages must be an array")
}

func TestTranslateRequest_MessageNotObject(t *testing.T) {
	body := map[string]any{
		"model":    "gemini-2.5-flash",
		"messages": []any{"not-a-map"},
	}
	_, _, _, err := NewVertexTranslator().TranslateRequest(body)
	assert.Error(t, err)
	assert.Contains(t, err.Error(), "not an object")
}

func TestTranslateRequest_ContentNonStringNonArray(t *testing.T) {
	body := map[string]any{
		"model": "gemini-2.5-flash",
		"messages": []any{
			map[string]any{"role": "user", "content": 42},
		},
	}
	translated, _, _, err := NewVertexTranslator().TranslateRequest(body)
	require.NoError(t, err)

	contents := translated["contents"].([]map[string]any)
	parts := contents[0]["parts"].([]map[string]any)
	assert.Equal(t, "", parts[0]["text"])
}

func TestTranslateRequest_ContentKeyMissing(t *testing.T) {
	body := map[string]any{
		"model": "gemini-2.5-flash",
		"messages": []any{
			map[string]any{"role": "user"},
		},
	}
	translated, _, _, err := NewVertexTranslator().TranslateRequest(body)
	require.NoError(t, err)

	contents := translated["contents"].([]map[string]any)
	parts := contents[0]["parts"].([]map[string]any)
	assert.Equal(t, "", parts[0]["text"])
}

func TestTranslateRequest_StopNonStringNonArray(t *testing.T) {
	body := map[string]any{
		"model":    "gemini-2.5-flash",
		"messages": []any{map[string]any{"role": "user", "content": "Hi"}},
		"stop":     12345,
	}
	translated, _, _, err := NewVertexTranslator().TranslateRequest(body)
	require.NoError(t, err)

	_, hasConfig := translated["generationConfig"]
	assert.False(t, hasConfig)
}

// --- Response translation tests ---

func TestTranslateResponse_BasicCompletion(t *testing.T) {
	body := map[string]any{
		"responseId":   "resp_123",
		"modelVersion": "gemini-2.5-flash",
		"candidates": []any{
			map[string]any{
				"content": map[string]any{
					"parts": []any{
						map[string]any{"text": "The answer is 4."},
					},
					"role": "model",
				},
				"finishReason": "STOP",
				"index":        float64(0),
			},
		},
		"usageMetadata": map[string]any{
			"promptTokenCount":     float64(10),
			"candidatesTokenCount": float64(5),
			"totalTokenCount":      float64(15),
		},
	}

	translated, err := NewVertexTranslator().TranslateResponse(body, "gemini-2.5-flash")
	require.NoError(t, err)

	assert.Equal(t, "resp_123", translated["id"])
	assert.Equal(t, "chat.completion", translated["object"])
	assert.Equal(t, "gemini-2.5-flash", translated["model"])

	choices := translated["choices"].([]any)
	require.Len(t, choices, 1)

	choice := choices[0].(map[string]any)
	assert.Equal(t, 0, choice["index"])
	assert.Equal(t, "stop", choice["finish_reason"])

	msg := choice["message"].(map[string]any)
	assert.Equal(t, "assistant", msg["role"])
	assert.Equal(t, "The answer is 4.", msg["content"])

	usage := translated["usage"].(map[string]any)
	assert.Equal(t, 10, usage["prompt_tokens"])
	assert.Equal(t, 5, usage["completion_tokens"])
	assert.Equal(t, 15, usage["total_tokens"])
}

func TestTranslateResponse_FinishReasons(t *testing.T) {
	tests := []struct {
		vertexReason string
		openaiReason string
	}{
		{"STOP", "stop"},
		{"MAX_TOKENS", "length"},
		{"SAFETY", "content_filter"},
		{"RECITATION", "content_filter"},
		{"BLOCKLIST", "content_filter"},
		{"PROHIBITED_CONTENT", "content_filter"},
		{"SPII", "content_filter"},
		{"MALFORMED_FUNCTION_CALL", "tool_calls"},
		{"UNEXPECTED_TOOL_CALL", "tool_calls"},
		{"MODEL_ARMOR", "content_filter"},
		{"IMAGE_SAFETY", "content_filter"},
		{"IMAGE_PROHIBITED_CONTENT", "content_filter"},
		{"IMAGE_RECITATION", "content_filter"},
		{"OTHER", "stop"},
		{"", "stop"},
	}

	for _, tt := range tests {
		t.Run(tt.vertexReason, func(t *testing.T) {
			body := map[string]any{
				"candidates": []any{
					map[string]any{
						"content": map[string]any{
							"parts": []any{map[string]any{"text": "hi"}},
							"role":  "model",
						},
						"finishReason": tt.vertexReason,
					},
				},
				"usageMetadata": map[string]any{
					"promptTokenCount":     float64(1),
					"candidatesTokenCount": float64(1),
					"totalTokenCount":      float64(2),
				},
			}

			translated, err := NewVertexTranslator().TranslateResponse(body, "test")
			require.NoError(t, err)

			choices := translated["choices"].([]any)
			choice := choices[0].(map[string]any)
			assert.Equal(t, tt.openaiReason, choice["finish_reason"])
		})
	}
}

func TestTranslateResponse_MultipleCandidates(t *testing.T) {
	body := map[string]any{
		"candidates": []any{
			map[string]any{
				"content":      map[string]any{"parts": []any{map[string]any{"text": "Response A"}}, "role": "model"},
				"finishReason": "STOP",
			},
			map[string]any{
				"content":      map[string]any{"parts": []any{map[string]any{"text": "Response B"}}, "role": "model"},
				"finishReason": "STOP",
			},
		},
		"usageMetadata": map[string]any{"promptTokenCount": float64(5), "candidatesTokenCount": float64(10), "totalTokenCount": float64(15)},
	}

	translated, err := NewVertexTranslator().TranslateResponse(body, "gemini-2.5-flash")
	require.NoError(t, err)

	choices := translated["choices"].([]any)
	require.Len(t, choices, 2)
	assert.Equal(t, 0, choices[0].(map[string]any)["index"])
	assert.Equal(t, 1, choices[1].(map[string]any)["index"])
	assert.Equal(t, "Response A", choices[0].(map[string]any)["message"].(map[string]any)["content"])
	assert.Equal(t, "Response B", choices[1].(map[string]any)["message"].(map[string]any)["content"])
}

func TestTranslateResponse_MultipleContentParts(t *testing.T) {
	body := map[string]any{
		"candidates": []any{
			map[string]any{
				"content":      map[string]any{"parts": []any{map[string]any{"text": "Hello "}, map[string]any{"text": "World"}}, "role": "model"},
				"finishReason": "STOP",
			},
		},
		"usageMetadata": map[string]any{"promptTokenCount": float64(1), "candidatesTokenCount": float64(2), "totalTokenCount": float64(3)},
	}

	translated, err := NewVertexTranslator().TranslateResponse(body, "test")
	require.NoError(t, err)

	msg := translated["choices"].([]any)[0].(map[string]any)["message"].(map[string]any)
	assert.Equal(t, "Hello World", msg["content"])
}

func TestTranslateResponse_ModelFromBody(t *testing.T) {
	body := map[string]any{
		"modelVersion": "gemini-2.5-flash",
		"candidates": []any{
			map[string]any{
				"content":      map[string]any{"parts": []any{map[string]any{"text": "hi"}}, "role": "model"},
				"finishReason": "STOP",
			},
		},
		"usageMetadata": map[string]any{"promptTokenCount": float64(1), "candidatesTokenCount": float64(1), "totalTokenCount": float64(2)},
	}

	translated, err := NewVertexTranslator().TranslateResponse(body, "")
	require.NoError(t, err)
	assert.Equal(t, "gemini-2.5-flash", translated["model"])
}

func TestTranslateResponse_ModelFromParam(t *testing.T) {
	body := map[string]any{
		"modelVersion": "gemini-2.5-flash",
		"candidates": []any{
			map[string]any{
				"content":      map[string]any{"parts": []any{map[string]any{"text": "hi"}}, "role": "model"},
				"finishReason": "STOP",
			},
		},
		"usageMetadata": map[string]any{"promptTokenCount": float64(1), "candidatesTokenCount": float64(1), "totalTokenCount": float64(2)},
	}

	translated, err := NewVertexTranslator().TranslateResponse(body, "my-model")
	require.NoError(t, err)
	assert.Equal(t, "my-model", translated["model"])
}

func TestTranslateResponse_MissingUsage(t *testing.T) {
	body := map[string]any{
		"candidates": []any{
			map[string]any{
				"content":      map[string]any{"parts": []any{map[string]any{"text": "hi"}}, "role": "model"},
				"finishReason": "STOP",
			},
		},
	}

	translated, err := NewVertexTranslator().TranslateResponse(body, "test")
	require.NoError(t, err)

	usage := translated["usage"].(map[string]any)
	assert.Equal(t, 0, usage["prompt_tokens"])
	assert.Equal(t, 0, usage["completion_tokens"])
	assert.Equal(t, 0, usage["total_tokens"])
}

func TestTranslateResponse_NoCandidates(t *testing.T) {
	body := map[string]any{
		"usageMetadata": map[string]any{"promptTokenCount": float64(5), "totalTokenCount": float64(5)},
	}

	translated, err := NewVertexTranslator().TranslateResponse(body, "test")
	require.NoError(t, err)

	choices := translated["choices"].([]any)
	assert.Empty(t, choices)
}

func TestTranslateResponse_EmptyCandidatesArray(t *testing.T) {
	body := map[string]any{
		"candidates":    []any{},
		"usageMetadata": map[string]any{"promptTokenCount": float64(5), "totalTokenCount": float64(5)},
	}

	translated, err := NewVertexTranslator().TranslateResponse(body, "test")
	require.NoError(t, err)

	choices := translated["choices"].([]any)
	assert.Empty(t, choices)
}

func TestTranslateResponse_VertexError(t *testing.T) {
	body := map[string]any{
		"error": map[string]any{
			"code":    float64(400),
			"message": "Invalid argument: model not found",
			"status":  "INVALID_ARGUMENT",
		},
	}

	translated, err := NewVertexTranslator().TranslateResponse(body, "gemini-2.5-flash")
	require.NoError(t, err)

	errObj := translated["error"].(map[string]any)
	assert.Equal(t, "INVALID_ARGUMENT", errObj["type"])
	assert.Equal(t, "Invalid argument: model not found", errObj["message"])
	assert.Equal(t, "400", errObj["code"])
}

func TestTranslateResponse_VertexError_NilCode(t *testing.T) {
	body := map[string]any{
		"error": map[string]any{
			"message": "Something went wrong",
			"status":  "INTERNAL",
		},
	}
	translated, err := NewVertexTranslator().TranslateResponse(body, "test")
	require.NoError(t, err)

	errObj := translated["error"].(map[string]any)
	assert.Equal(t, "", errObj["code"])
	assert.Equal(t, "INTERNAL", errObj["type"])
}

func TestTranslateResponse_VertexError_404(t *testing.T) {
	body := map[string]any{
		"error": map[string]any{
			"code":    float64(404),
			"message": "models/nonexistent-model is not found",
			"status":  "NOT_FOUND",
		},
	}

	translated, err := NewVertexTranslator().TranslateResponse(body, "test")
	require.NoError(t, err)

	errObj := translated["error"].(map[string]any)
	assert.Equal(t, "NOT_FOUND", errObj["type"])
	assert.Equal(t, "404", errObj["code"])
	assert.Nil(t, errObj["param"])
}

func TestTranslateResponse_VertexError_429(t *testing.T) {
	body := map[string]any{
		"error": map[string]any{
			"code":    float64(429),
			"message": "Resource has been exhausted",
			"status":  "RESOURCE_EXHAUSTED",
		},
	}

	translated, err := NewVertexTranslator().TranslateResponse(body, "test")
	require.NoError(t, err)

	errObj := translated["error"].(map[string]any)
	assert.Equal(t, "RESOURCE_EXHAUSTED", errObj["type"])
	assert.Equal(t, "429", errObj["code"])
}

func TestTranslateResponse_FunctionCall(t *testing.T) {
	body := map[string]any{
		"candidates": []any{
			map[string]any{
				"content": map[string]any{
					"parts": []any{
						map[string]any{"text": "I'll check the weather."},
						map[string]any{
							"functionCall": map[string]any{
								"name": "get_weather",
								"args": map[string]any{"location": "San Francisco"},
							},
						},
					},
					"role": "model",
				},
				"finishReason": "STOP",
			},
		},
		"usageMetadata": map[string]any{"promptTokenCount": float64(20), "candidatesTokenCount": float64(15), "totalTokenCount": float64(35)},
	}

	translated, err := NewVertexTranslator().TranslateResponse(body, "gemini-2.5-flash")
	require.NoError(t, err)

	choices := translated["choices"].([]any)
	choice := choices[0].(map[string]any)
	msg := choice["message"].(map[string]any)

	assert.Equal(t, "I'll check the weather.", msg["content"])

	toolCalls := msg["tool_calls"].([]any)
	require.Len(t, toolCalls, 1)

	tc := toolCalls[0].(map[string]any)
	assert.Equal(t, "call_0", tc["id"])
	assert.Equal(t, "function", tc["type"])
	assert.Equal(t, 0, tc["index"])

	fn := tc["function"].(map[string]any)
	assert.Equal(t, "get_weather", fn["name"])
	assert.JSONEq(t, `{"location":"San Francisco"}`, fn["arguments"].(string))
}

func TestTranslateResponse_MultipleFunctionCalls(t *testing.T) {
	body := map[string]any{
		"candidates": []any{
			map[string]any{
				"content": map[string]any{
					"parts": []any{
						map[string]any{"functionCall": map[string]any{"name": "get_weather", "args": map[string]any{"location": "NYC"}}},
						map[string]any{"functionCall": map[string]any{"name": "get_time", "args": map[string]any{"timezone": "EST"}}},
					},
					"role": "model",
				},
				"finishReason": "STOP",
			},
		},
		"usageMetadata": map[string]any{"promptTokenCount": float64(10), "candidatesTokenCount": float64(20), "totalTokenCount": float64(30)},
	}

	translated, err := NewVertexTranslator().TranslateResponse(body, "gemini-2.5-flash")
	require.NoError(t, err)

	msg := translated["choices"].([]any)[0].(map[string]any)["message"].(map[string]any)
	toolCalls := msg["tool_calls"].([]any)
	require.Len(t, toolCalls, 2)

	assert.Equal(t, "call_0", toolCalls[0].(map[string]any)["id"])
	assert.Equal(t, 0, toolCalls[0].(map[string]any)["index"])
	assert.Equal(t, "call_1", toolCalls[1].(map[string]any)["id"])
	assert.Equal(t, 1, toolCalls[1].(map[string]any)["index"])
}

func TestTranslateResponse_FunctionCallStringArgs(t *testing.T) {
	body := map[string]any{
		"candidates": []any{map[string]any{
			"content": map[string]any{
				"parts": []any{map[string]any{"functionCall": map[string]any{"name": "fn", "args": `{"key":"value"}`}}},
				"role":  "model",
			},
			"finishReason": "STOP",
		}},
		"usageMetadata": map[string]any{"promptTokenCount": float64(1), "candidatesTokenCount": float64(1), "totalTokenCount": float64(2)},
	}
	translated, err := NewVertexTranslator().TranslateResponse(body, "test")
	require.NoError(t, err)

	tc := translated["choices"].([]any)[0].(map[string]any)["message"].(map[string]any)["tool_calls"].([]any)[0].(map[string]any)
	assert.Equal(t, `{"key":"value"}`, tc["function"].(map[string]any)["arguments"])
}

func TestTranslateResponse_FunctionCallNilArgs(t *testing.T) {
	body := map[string]any{
		"candidates": []any{map[string]any{
			"content": map[string]any{
				"parts": []any{map[string]any{"functionCall": map[string]any{"name": "fn", "args": nil}}},
				"role":  "model",
			},
			"finishReason": "STOP",
		}},
		"usageMetadata": map[string]any{"promptTokenCount": float64(1), "candidatesTokenCount": float64(1), "totalTokenCount": float64(2)},
	}
	translated, err := NewVertexTranslator().TranslateResponse(body, "test")
	require.NoError(t, err)

	tc := translated["choices"].([]any)[0].(map[string]any)["message"].(map[string]any)["tool_calls"].([]any)[0].(map[string]any)
	assert.Equal(t, "{}", tc["function"].(map[string]any)["arguments"])
}

func TestTranslateResponse_FunctionCallUnmarshalableArgs(t *testing.T) {
	body := map[string]any{
		"candidates": []any{map[string]any{
			"content": map[string]any{
				"parts": []any{map[string]any{"functionCall": map[string]any{"name": "fn", "args": make(chan int)}}},
				"role":  "model",
			},
			"finishReason": "STOP",
		}},
		"usageMetadata": map[string]any{"promptTokenCount": float64(1), "candidatesTokenCount": float64(1), "totalTokenCount": float64(2)},
	}
	translated, err := NewVertexTranslator().TranslateResponse(body, "test")
	require.NoError(t, err)

	msg := translated["choices"].([]any)[0].(map[string]any)["message"].(map[string]any)
	assert.Nil(t, msg["tool_calls"])
}

func TestTranslateResponse_CandidateNotMap(t *testing.T) {
	body := map[string]any{
		"candidates":    []any{"not-a-map"},
		"usageMetadata": map[string]any{"promptTokenCount": float64(1), "candidatesTokenCount": float64(1), "totalTokenCount": float64(2)},
	}
	translated, err := NewVertexTranslator().TranslateResponse(body, "test")
	require.NoError(t, err)

	choices := translated["choices"].([]any)
	assert.Empty(t, choices)
}

func TestTranslateResponse_CandidateNoContent(t *testing.T) {
	body := map[string]any{
		"candidates":    []any{map[string]any{"finishReason": "STOP"}},
		"usageMetadata": map[string]any{"promptTokenCount": float64(1), "candidatesTokenCount": float64(1), "totalTokenCount": float64(2)},
	}
	translated, err := NewVertexTranslator().TranslateResponse(body, "test")
	require.NoError(t, err)

	msg := translated["choices"].([]any)[0].(map[string]any)["message"].(map[string]any)
	assert.Equal(t, "", msg["content"])
	assert.Nil(t, msg["tool_calls"])
}

func TestTranslateResponse_PartsNotArray(t *testing.T) {
	body := map[string]any{
		"candidates": []any{map[string]any{
			"content":      map[string]any{"parts": "not-an-array", "role": "model"},
			"finishReason": "STOP",
		}},
		"usageMetadata": map[string]any{"promptTokenCount": float64(1), "candidatesTokenCount": float64(1), "totalTokenCount": float64(2)},
	}
	translated, err := NewVertexTranslator().TranslateResponse(body, "test")
	require.NoError(t, err)

	msg := translated["choices"].([]any)[0].(map[string]any)["message"].(map[string]any)
	assert.Equal(t, "", msg["content"])
}

func TestTranslateResponse_PartNotMap(t *testing.T) {
	body := map[string]any{
		"candidates": []any{map[string]any{
			"content":      map[string]any{"parts": []any{"not-a-map", map[string]any{"text": "hello"}}, "role": "model"},
			"finishReason": "STOP",
		}},
		"usageMetadata": map[string]any{"promptTokenCount": float64(1), "candidatesTokenCount": float64(1), "totalTokenCount": float64(2)},
	}
	translated, err := NewVertexTranslator().TranslateResponse(body, "test")
	require.NoError(t, err)

	msg := translated["choices"].([]any)[0].(map[string]any)["message"].(map[string]any)
	assert.Equal(t, "hello", msg["content"])
}

func TestTranslateResponse_UnknownFieldsIgnored(t *testing.T) {
	body := map[string]any{
		"candidates": []any{
			map[string]any{
				"content":          map[string]any{"parts": []any{map[string]any{"text": "hi"}}, "role": "model"},
				"finishReason":     "STOP",
				"safetyRatings":    []any{map[string]any{"category": "HARM_CATEGORY_HARASSMENT", "probability": "NEGLIGIBLE"}},
				"citationMetadata": map[string]any{},
			},
		},
		"usageMetadata": map[string]any{
			"promptTokenCount":     float64(1),
			"candidatesTokenCount": float64(1),
			"totalTokenCount":      float64(2),
			"promptTokensDetails":  []any{map[string]any{"modality": "TEXT", "tokenCount": float64(1)}},
			"thoughtsTokenCount":   float64(50),
		},
		"modelVersion": "gemini-2.5-flash",
		"responseId":   "abc123",
	}

	translated, err := NewVertexTranslator().TranslateResponse(body, "test")
	require.NoError(t, err)

	assert.Equal(t, "chat.completion", translated["object"])
	assert.Nil(t, translated["safetyRatings"])
	assert.Nil(t, translated["citationMetadata"])
	assert.Nil(t, translated["promptTokensDetails"])

	usage := translated["usage"].(map[string]any)
	assert.Equal(t, 1, usage["prompt_tokens"])
	assert.Equal(t, 1, usage["completion_tokens"])
	assert.Equal(t, 2, usage["total_tokens"])
}

func TestTranslateResponse_EmptyBody(t *testing.T) {
	body := map[string]any{}

	translated, err := NewVertexTranslator().TranslateResponse(body, "test")
	require.NoError(t, err)

	assert.Equal(t, "chat.completion", translated["object"])
	choices := translated["choices"].([]any)
	assert.Empty(t, choices)
	usage := translated["usage"].(map[string]any)
	assert.Equal(t, 0, usage["prompt_tokens"])
}

// --- Helper function edge case tests ---

func TestGetFloat_IntTypes(t *testing.T) {
	body := map[string]any{
		"model":       "gemini-2.5-flash",
		"messages":    []any{map[string]any{"role": "user", "content": "Hi"}},
		"temperature": int(1),
		"top_p":       int64(1),
	}
	translated, _, _, err := NewVertexTranslator().TranslateRequest(body)
	require.NoError(t, err)

	config := translated["generationConfig"].(map[string]any)
	assert.Equal(t, float64(1), config["temperature"])
	assert.Equal(t, float64(1), config["topP"])
}

func TestGetFloat_UnsupportedType(t *testing.T) {
	body := map[string]any{
		"model":       "gemini-2.5-flash",
		"messages":    []any{map[string]any{"role": "user", "content": "Hi"}},
		"temperature": "not-a-number",
	}
	translated, _, _, err := NewVertexTranslator().TranslateRequest(body)
	require.NoError(t, err)

	_, hasConfig := translated["generationConfig"]
	assert.False(t, hasConfig)
}

func TestGetInt_IntTypes(t *testing.T) {
	tests := []struct {
		name  string
		value any
	}{
		{"int", int(300)},
		{"int64", int64(300)},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			body := map[string]any{
				"model":      "gemini-2.5-flash",
				"messages":   []any{map[string]any{"role": "user", "content": "Hi"}},
				"max_tokens": tt.value,
			}
			translated, _, _, err := NewVertexTranslator().TranslateRequest(body)
			require.NoError(t, err)
			config := translated["generationConfig"].(map[string]any)
			assert.Equal(t, 300, config["maxOutputTokens"])
		})
	}
}

func TestGetInt_UnsupportedType(t *testing.T) {
	body := map[string]any{
		"model":      "gemini-2.5-flash",
		"messages":   []any{map[string]any{"role": "user", "content": "Hi"}},
		"max_tokens": "not-a-number",
	}
	translated, _, _, err := NewVertexTranslator().TranslateRequest(body)
	require.NoError(t, err)

	_, hasConfig := translated["generationConfig"]
	assert.False(t, hasConfig)
}

func TestToInt_IntTypes(t *testing.T) {
	body := map[string]any{
		"candidates": []any{map[string]any{
			"content":      map[string]any{"parts": []any{map[string]any{"text": "hi"}}, "role": "model"},
			"finishReason": "STOP",
		}},
		"usageMetadata": map[string]any{
			"promptTokenCount":     int(10),
			"candidatesTokenCount": int64(5),
			"totalTokenCount":      "not-a-number",
		},
	}
	translated, err := NewVertexTranslator().TranslateResponse(body, "test")
	require.NoError(t, err)

	usage := translated["usage"].(map[string]any)
	assert.Equal(t, 10, usage["prompt_tokens"])
	assert.Equal(t, 5, usage["completion_tokens"])
	assert.Equal(t, 0, usage["total_tokens"])
}
