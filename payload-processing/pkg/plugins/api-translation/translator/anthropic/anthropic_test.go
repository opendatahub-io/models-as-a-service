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

package anthropic

import (
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestTranslateRequest_BasicChat(t *testing.T) {
	body := map[string]any{
		"model": "claude-sonnet-4-20250514",
		"messages": []any{
			map[string]any{"role": "user", "content": "What is 2+2?"},
		},
	}

	translated, headers, headersToRemove, err := NewAnthropicTranslator().TranslateRequest(body)
	require.NoError(t, err)

	assert.Equal(t, "claude-sonnet-4-20250514", translated["model"])
	assert.Equal(t, defaultMaxTokens, translated["max_tokens"])

	msgs := translated["messages"].([]map[string]any)
	require.Len(t, msgs, 1)
	assert.Equal(t, "user", msgs[0]["role"])
	assert.Equal(t, "What is 2+2?", msgs[0]["content"])

	assert.Nil(t, translated["system"])

	assert.Equal(t, anthropicAPIVersion, headers["anthropic-version"])
	assert.Equal(t, "application/json", headers["content-type"])
	assert.Equal(t, anthropicPath, headers[":path"])

	assert.Empty(t, headersToRemove)
}

func TestTranslateRequest_SystemMessage(t *testing.T) {
	body := map[string]any{
		"model": "claude-sonnet-4-20250514",
		"messages": []any{
			map[string]any{"role": "system", "content": "You are a helpful assistant."},
			map[string]any{"role": "user", "content": "Hello"},
		},
	}

	translated, _, _, err := NewAnthropicTranslator().TranslateRequest(body)
	require.NoError(t, err)

	assert.Equal(t, "You are a helpful assistant.", translated["system"])

	msgs := translated["messages"].([]map[string]any)
	require.Len(t, msgs, 1)
	assert.Equal(t, "user", msgs[0]["role"])
}

func TestTranslateRequest_MultipleMessages(t *testing.T) {
	body := map[string]any{
		"model": "claude-sonnet-4-20250514",
		"messages": []any{
			map[string]any{"role": "user", "content": "Hi"},
			map[string]any{"role": "assistant", "content": "Hello!"},
			map[string]any{"role": "user", "content": "How are you?"},
		},
	}

	translated, _, _, err := NewAnthropicTranslator().TranslateRequest(body)
	require.NoError(t, err)

	msgs := translated["messages"].([]map[string]any)
	require.Len(t, msgs, 3)
	assert.Equal(t, "user", msgs[0]["role"])
	assert.Equal(t, "assistant", msgs[1]["role"])
	assert.Equal(t, "user", msgs[2]["role"])
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
				"model":                 "claude-sonnet-4-20250514",
				"messages":              []any{map[string]any{"role": "user", "content": "Hi"}},
				"max_completion_tokens": float64(200),
				"max_tokens":            float64(100),
			},
			expected: 200,
		},
		{
			name: "max_tokens fallback",
			body: map[string]any{
				"model":      "claude-sonnet-4-20250514",
				"messages":   []any{map[string]any{"role": "user", "content": "Hi"}},
				"max_tokens": float64(500),
			},
			expected: 500,
		},
		{
			name: "default when neither set",
			body: map[string]any{
				"model":    "claude-sonnet-4-20250514",
				"messages": []any{map[string]any{"role": "user", "content": "Hi"}},
			},
			expected: defaultMaxTokens,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			translated, _, _, err := NewAnthropicTranslator().TranslateRequest(tt.body)
			require.NoError(t, err)
			assert.Equal(t, tt.expected, translated["max_tokens"])
		})
	}
}

func TestTranslateRequest_OptionalParams(t *testing.T) {
	body := map[string]any{
		"model":       "claude-sonnet-4-20250514",
		"messages":    []any{map[string]any{"role": "user", "content": "Hi"}},
		"temperature": 0.7,
		"top_p":       0.9,
		"stop":        []any{"END", "STOP"},
	}

	translated, _, _, err := NewAnthropicTranslator().TranslateRequest(body)
	require.NoError(t, err)

	assert.Equal(t, 0.7, translated["temperature"])
	assert.Equal(t, 0.9, translated["top_p"])
	assert.Equal(t, []string{"END", "STOP"}, translated["stop_sequences"])
}

func TestTranslateRequest_StopString(t *testing.T) {
	body := map[string]any{
		"model":    "claude-sonnet-4-20250514",
		"messages": []any{map[string]any{"role": "user", "content": "Hi"}},
		"stop":     "END",
	}

	translated, _, _, err := NewAnthropicTranslator().TranslateRequest(body)
	require.NoError(t, err)

	assert.Equal(t, []string{"END"}, translated["stop_sequences"])
}

func TestTranslateRequest_MissingModel(t *testing.T) {
	body := map[string]any{
		"messages": []any{map[string]any{"role": "user", "content": "Hi"}},
	}

	_, _, _, err := NewAnthropicTranslator().TranslateRequest(body)
	assert.Error(t, err)
	assert.Contains(t, err.Error(), "model")
}

func TestTranslateRequest_MissingMessages(t *testing.T) {
	body := map[string]any{
		"model": "claude-sonnet-4-20250514",
	}

	_, _, _, err := NewAnthropicTranslator().TranslateRequest(body)
	assert.Error(t, err)
	assert.Contains(t, err.Error(), "messages")
}

func TestTranslateRequest_OnlySystemMessage(t *testing.T) {
	body := map[string]any{
		"model": "claude-sonnet-4-20250514",
		"messages": []any{
			map[string]any{"role": "system", "content": "You are helpful"},
		},
	}

	_, _, _, err := NewAnthropicTranslator().TranslateRequest(body)
	assert.Error(t, err)
	assert.Contains(t, err.Error(), "non-system message")
}

func TestTranslateRequest_ContentParts(t *testing.T) {
	body := map[string]any{
		"model": "claude-sonnet-4-20250514",
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

	translated, _, _, err := NewAnthropicTranslator().TranslateRequest(body)
	require.NoError(t, err)

	msgs := translated["messages"].([]map[string]any)
	assert.Equal(t, "Hello World", msgs[0]["content"])
}

func TestTranslateResponse_BasicCompletion(t *testing.T) {
	body := map[string]any{
		"id":    "msg_123",
		"type":  "message",
		"model": "claude-sonnet-4-20250514",
		"content": []any{
			map[string]any{"type": "text", "text": "The answer is 4."},
		},
		"stop_reason": "end_turn",
		"usage": map[string]any{
			"input_tokens":  float64(10),
			"output_tokens": float64(5),
		},
	}

	translated, err := NewAnthropicTranslator().TranslateResponse(body, "claude-sonnet-4-20250514")
	require.NoError(t, err)

	assert.Equal(t, "msg_123", translated["id"])
	assert.Equal(t, "chat.completion", translated["object"])
	assert.Equal(t, "claude-sonnet-4-20250514", translated["model"])

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

func TestTranslateResponse_StopReasons(t *testing.T) {
	tests := []struct {
		anthropicReason string
		openaiReason    string
	}{
		{"end_turn", "stop"},
		{"max_tokens", "length"},
		{"tool_use", "tool_calls"},
		{"", "stop"},
	}

	for _, tt := range tests {
		t.Run(tt.anthropicReason, func(t *testing.T) {
			body := map[string]any{
				"type":        "message",
				"content":     []any{map[string]any{"type": "text", "text": "hi"}},
				"stop_reason": tt.anthropicReason,
				"usage":       map[string]any{"input_tokens": float64(1), "output_tokens": float64(1)},
			}

			translated, err := NewAnthropicTranslator().TranslateResponse(body, "test")
			require.NoError(t, err)

			choices := translated["choices"].([]any)
			choice := choices[0].(map[string]any)
			assert.Equal(t, tt.openaiReason, choice["finish_reason"])
		})
	}
}

func TestTranslateResponse_MultipleContentBlocks(t *testing.T) {
	body := map[string]any{
		"type": "message",
		"content": []any{
			map[string]any{"type": "text", "text": "Hello "},
			map[string]any{"type": "text", "text": "World"},
		},
		"stop_reason": "end_turn",
		"usage":       map[string]any{"input_tokens": float64(1), "output_tokens": float64(2)},
	}

	translated, err := NewAnthropicTranslator().TranslateResponse(body, "test")
	require.NoError(t, err)

	choices := translated["choices"].([]any)
	msg := choices[0].(map[string]any)["message"].(map[string]any)
	assert.Equal(t, "Hello World", msg["content"])
}

func TestTranslateResponse_ModelFromBody(t *testing.T) {
	body := map[string]any{
		"type":        "message",
		"model":       "claude-sonnet-4-20250514",
		"content":     []any{map[string]any{"type": "text", "text": "hi"}},
		"stop_reason": "end_turn",
		"usage":       map[string]any{"input_tokens": float64(1), "output_tokens": float64(1)},
	}

	translated, err := NewAnthropicTranslator().TranslateResponse(body, "")
	require.NoError(t, err)
	assert.Equal(t, "claude-sonnet-4-20250514", translated["model"])
}

func TestTranslateResponse_MissingUsage(t *testing.T) {
	body := map[string]any{
		"type":        "message",
		"content":     []any{map[string]any{"type": "text", "text": "hi"}},
		"stop_reason": "end_turn",
	}

	translated, err := NewAnthropicTranslator().TranslateResponse(body, "test")
	require.NoError(t, err)

	usage := translated["usage"].(map[string]any)
	assert.Equal(t, 0, usage["prompt_tokens"])
	assert.Equal(t, 0, usage["completion_tokens"])
	assert.Equal(t, 0, usage["total_tokens"])
}

func TestTranslateResponse_AnthropicError(t *testing.T) {
	body := map[string]any{
		"type": "error",
		"error": map[string]any{
			"type":    "invalid_request_error",
			"message": "max_tokens must be a positive integer",
		},
	}

	translated, err := NewAnthropicTranslator().TranslateResponse(body, "claude-sonnet-4-20250514")
	require.NoError(t, err)

	errObj := translated["error"].(map[string]any)
	assert.Equal(t, "invalid_request_error", errObj["type"])
	assert.Equal(t, "max_tokens must be a positive integer", errObj["message"])
	assert.Equal(t, "invalid_request_error", errObj["code"])
}

func TestTranslateResponse_ToolUse(t *testing.T) {
	body := map[string]any{
		"id":   "msg_123",
		"type": "message",
		"content": []any{
			map[string]any{"type": "text", "text": "I'll check the weather."},
			map[string]any{
				"type":  "tool_use",
				"id":    "toolu_abc",
				"name":  "get_weather",
				"input": map[string]any{"location": "San Francisco"},
			},
		},
		"stop_reason": "tool_use",
		"usage":       map[string]any{"input_tokens": float64(20), "output_tokens": float64(15)},
	}

	translated, err := NewAnthropicTranslator().TranslateResponse(body, "claude-sonnet-4-20250514")
	require.NoError(t, err)

	choices := translated["choices"].([]any)
	choice := choices[0].(map[string]any)
	assert.Equal(t, "tool_calls", choice["finish_reason"])

	msg := choice["message"].(map[string]any)
	assert.Equal(t, "I'll check the weather.", msg["content"])

	toolCalls := msg["tool_calls"].([]any)
	require.Len(t, toolCalls, 1)

	tc := toolCalls[0].(map[string]any)
	assert.Equal(t, "toolu_abc", tc["id"])
	assert.Equal(t, "function", tc["type"])

	fn := tc["function"].(map[string]any)
	assert.Equal(t, "get_weather", fn["name"])
}

func TestTranslateRequest_DeveloperRole(t *testing.T) {
	body := map[string]any{
		"model": "claude-sonnet-4-20250514",
		"messages": []any{
			map[string]any{"role": "developer", "content": "You are a coding assistant."},
			map[string]any{"role": "user", "content": "Hello"},
		},
	}

	translated, _, _, err := NewAnthropicTranslator().TranslateRequest(body)
	require.NoError(t, err)

	assert.Equal(t, "You are a coding assistant.", translated["system"])

	msgs := translated["messages"].([]map[string]any)
	require.Len(t, msgs, 1)
	assert.Equal(t, "user", msgs[0]["role"])
}

func TestTranslateRequest_SystemAndDeveloperConcatenated(t *testing.T) {
	body := map[string]any{
		"model": "claude-sonnet-4-20250514",
		"messages": []any{
			map[string]any{"role": "system", "content": "Be concise."},
			map[string]any{"role": "developer", "content": "Use markdown."},
			map[string]any{"role": "user", "content": "Hello"},
		},
	}

	translated, _, _, err := NewAnthropicTranslator().TranslateRequest(body)
	require.NoError(t, err)

	assert.Equal(t, "Be concise.\nUse markdown.", translated["system"])
}

func TestTranslateRequest_ToolRoleRejected(t *testing.T) {
	body := map[string]any{
		"model": "claude-sonnet-4-20250514",
		"messages": []any{
			map[string]any{"role": "user", "content": "Hi"},
			map[string]any{"role": "tool", "content": "result", "tool_call_id": "abc"},
		},
	}

	_, _, _, err := NewAnthropicTranslator().TranslateRequest(body)
	assert.Error(t, err)
	assert.Contains(t, err.Error(), "tool")
	assert.Contains(t, err.Error(), "not supported")
}

func TestTranslateRequest_UnknownRoleRejected(t *testing.T) {
	body := map[string]any{
		"model": "claude-sonnet-4-20250514",
		"messages": []any{
			map[string]any{"role": "narrator", "content": "Once upon a time"},
		},
	}

	_, _, _, err := NewAnthropicTranslator().TranslateRequest(body)
	assert.Error(t, err)
	assert.Contains(t, err.Error(), "unknown role")
}

func TestTranslateRequest_NonTextContentSkipped(t *testing.T) {
	body := map[string]any{
		"model": "claude-sonnet-4-20250514",
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

	translated, _, _, err := NewAnthropicTranslator().TranslateRequest(body)
	require.NoError(t, err)

	msgs := translated["messages"].([]map[string]any)
	assert.Equal(t, "Describe this", msgs[0]["content"])
}
