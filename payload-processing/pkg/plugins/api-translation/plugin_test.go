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

package api_translation

import (
	"context"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"sigs.k8s.io/gateway-api-inference-extension/pkg/bbr/framework"

	"github.com/opendatahub-io/ai-gateway-payload-processing/pkg/plugins/common/state"
)

func newCycleStateWithProvider(providerName string) *framework.CycleState {
	cs := framework.NewCycleState()
	cs.Write(state.ProviderKey, providerName)
	return cs
}

func TestProcessRequest_NoProvider(t *testing.T) {
	p := NewAPITranslationPlugin()

	req := framework.NewInferenceRequest()
	req.Body["model"] = "gpt-4o"
	req.Body["messages"] = []any{map[string]any{"role": "user", "content": "Hi"}}

	err := p.ProcessRequest(context.Background(), framework.NewCycleState(), req)
	assert.NoError(t, err)
	assert.False(t, req.BodyMutated())
}

func TestProcessRequest_OpenAIProvider(t *testing.T) {
	p := NewAPITranslationPlugin()

	cs := newCycleStateWithProvider("openai")
	req := framework.NewInferenceRequest()
	req.Body["model"] = "gpt-4o"
	req.Body["messages"] = []any{map[string]any{"role": "user", "content": "Hi"}}

	err := p.ProcessRequest(context.Background(), cs, req)
	assert.NoError(t, err)
	assert.False(t, req.BodyMutated())
}

func TestProcessRequest_AnthropicProvider(t *testing.T) {
	p := NewAPITranslationPlugin()

	cs := newCycleStateWithProvider("anthropic")
	req := framework.NewInferenceRequest()
	req.Headers["authorization"] = "Bearer sk-test"
	req.Headers["content-length"] = "123"
	req.Body["model"] = "claude-sonnet-4-20250514"
	req.Body["messages"] = []any{
		map[string]any{"role": "system", "content": "Be concise"},
		map[string]any{"role": "user", "content": "What is 2+2?"},
	}
	req.Body["max_tokens"] = float64(100)

	err := p.ProcessRequest(context.Background(), cs, req)
	require.NoError(t, err)

	assert.True(t, req.BodyMutated())

	assert.Equal(t, "Be concise", req.Body["system"])
	assert.Equal(t, 100, req.Body["max_tokens"])

	msgs, ok := req.Body["messages"].([]map[string]any)
	require.True(t, ok)
	require.Len(t, msgs, 1)
	assert.Equal(t, "user", msgs[0]["role"])

	mutated := req.MutatedHeaders()
	assert.Equal(t, "2023-06-01", mutated["anthropic-version"])
	assert.Equal(t, "/v1/messages", mutated[":path"])
	assert.Equal(t, "application/json", mutated["content-type"])

	removed := req.RemovedHeaders()
	assert.Contains(t, removed, "authorization")
	assert.NotContains(t, removed, "content-length")
}

func TestProcessRequest_AzureOpenAIProvider(t *testing.T) {
	p := NewAPITranslationPlugin()

	cs := newCycleStateWithProvider("azure-openai")
	req := framework.NewInferenceRequest()
	req.Headers["authorization"] = "Bearer sk-test"
	req.Headers["content-length"] = "200"
	req.Body["model"] = "gpt-4o"
	req.Body["messages"] = []any{
		map[string]any{"role": "system", "content": "Be concise"},
		map[string]any{"role": "user", "content": "What is 2+2?"},
	}
	req.Body["max_tokens"] = float64(100)
	req.Body["temperature"] = 0.7

	err := p.ProcessRequest(context.Background(), cs, req)
	require.NoError(t, err)

	// Azure OpenAI does not mutate the body — same schema as OpenAI
	assert.False(t, req.BodyMutated())

	// Original body fields are preserved
	assert.Equal(t, "gpt-4o", req.Body["model"])
	assert.Equal(t, float64(100), req.Body["max_tokens"])
	assert.Equal(t, 0.7, req.Body["temperature"])

	mutated := req.MutatedHeaders()
	assert.Equal(t, "/openai/deployments/gpt-4o/chat/completions?api-version=2024-10-21", mutated[":path"])
	assert.Equal(t, "application/json", mutated["content-type"])

	removed := req.RemovedHeaders()
	assert.Contains(t, removed, "authorization")
	assert.NotContains(t, removed, "content-length")
}

func TestProcessResponse_AzureOpenAI(t *testing.T) {
	p := NewAPITranslationPlugin()

	cs := newCycleStateWithProvider("azure-openai")
	cs.Write(state.ModelKey, "gpt-4o")

	resp := framework.NewInferenceResponse()
	resp.Body["id"] = "chatcmpl-abc123"
	resp.Body["object"] = "chat.completion"
	resp.Body["model"] = "gpt-4o"
	resp.Body["choices"] = []any{
		map[string]any{
			"index": float64(0),
			"message": map[string]any{
				"role":    "assistant",
				"content": "The answer is 4.",
			},
			"finish_reason": "stop",
		},
	}
	resp.Body["usage"] = map[string]any{
		"prompt_tokens":     float64(10),
		"completion_tokens": float64(5),
		"total_tokens":      float64(15),
	}

	err := p.ProcessResponse(context.Background(), cs, resp)
	require.NoError(t, err)

	// Azure OpenAI responses are already in OpenAI format — no mutation
	assert.False(t, resp.BodyMutated())
}

func TestProcessRequest_UnknownProvider(t *testing.T) {
	p := NewAPITranslationPlugin()

	cs := newCycleStateWithProvider("unknown")
	req := framework.NewInferenceRequest()
	req.Body["model"] = "some-model"
	req.Body["messages"] = []any{map[string]any{"role": "user", "content": "Hi"}}

	err := p.ProcessRequest(context.Background(), cs, req)
	assert.Error(t, err)
	assert.Contains(t, err.Error(), "unsupported provider")
	assert.Contains(t, err.Error(), "unknown")
}

func TestProcessRequest_NilRequest(t *testing.T) {
	p := NewAPITranslationPlugin()

	err := p.ProcessRequest(context.Background(), framework.NewCycleState(), nil)
	assert.Error(t, err)
	assert.Contains(t, err.Error(), "non-nil")
}

func TestProcessResponse_NilResponse(t *testing.T) {
	p := NewAPITranslationPlugin()

	err := p.ProcessResponse(context.Background(), framework.NewCycleState(), nil)
	assert.Error(t, err)
	assert.Contains(t, err.Error(), "non-nil")
}

func TestProcessResponse_Anthropic(t *testing.T) {
	p := NewAPITranslationPlugin()

	cs := newCycleStateWithProvider("anthropic")
	cs.Write(state.ModelKey, "claude-sonnet-4-20250514")

	resp := framework.NewInferenceResponse()
	resp.Body["id"] = "msg_123"
	resp.Body["type"] = "message"
	resp.Body["content"] = []any{
		map[string]any{"type": "text", "text": "The answer is 4."},
	}
	resp.Body["stop_reason"] = "end_turn"
	resp.Body["usage"] = map[string]any{
		"input_tokens":  float64(10),
		"output_tokens": float64(5),
	}

	err := p.ProcessResponse(context.Background(), cs, resp)
	require.NoError(t, err)

	assert.True(t, resp.BodyMutated())
	assert.Equal(t, "chat.completion", resp.Body["object"])
	assert.Equal(t, "claude-sonnet-4-20250514", resp.Body["model"])

	choices := resp.Body["choices"].([]any)
	choice := choices[0].(map[string]any)
	msg := choice["message"].(map[string]any)
	assert.Equal(t, "assistant", msg["role"])
	assert.Equal(t, "The answer is 4.", msg["content"])
	assert.Equal(t, "stop", choice["finish_reason"])

	usage := resp.Body["usage"].(map[string]any)
	assert.Equal(t, 10, usage["prompt_tokens"])
	assert.Equal(t, 5, usage["completion_tokens"])
	assert.Equal(t, 15, usage["total_tokens"])
}

func TestProcessResponse_AnthropicError(t *testing.T) {
	p := NewAPITranslationPlugin()

	cs := newCycleStateWithProvider("anthropic")

	resp := framework.NewInferenceResponse()
	resp.Body["type"] = "error"
	resp.Body["error"] = map[string]any{
		"type":    "invalid_request_error",
		"message": "max_tokens must be positive",
	}

	err := p.ProcessResponse(context.Background(), cs, resp)
	require.NoError(t, err)

	assert.True(t, resp.BodyMutated())
	errObj := resp.Body["error"].(map[string]any)
	assert.Equal(t, "invalid_request_error", errObj["type"])
}

func TestProcessResponse_AnthropicToolUse(t *testing.T) {
	p := NewAPITranslationPlugin()

	cs := newCycleStateWithProvider("anthropic")
	cs.Write(state.ModelKey, "claude-sonnet-4-20250514")

	resp := framework.NewInferenceResponse()
	resp.Body["id"] = "msg_456"
	resp.Body["type"] = "message"
	resp.Body["content"] = []any{
		map[string]any{"type": "text", "text": "Let me check."},
		map[string]any{
			"type":  "tool_use",
			"id":    "toolu_abc",
			"name":  "get_weather",
			"input": map[string]any{"location": "Paris"},
		},
	}
	resp.Body["stop_reason"] = "tool_use"
	resp.Body["usage"] = map[string]any{
		"input_tokens":  float64(20),
		"output_tokens": float64(10),
	}

	err := p.ProcessResponse(context.Background(), cs, resp)
	require.NoError(t, err)

	assert.True(t, resp.BodyMutated())

	choices := resp.Body["choices"].([]any)
	choice := choices[0].(map[string]any)
	assert.Equal(t, "tool_calls", choice["finish_reason"])

	msg := choice["message"].(map[string]any)
	toolCalls := msg["tool_calls"].([]any)
	require.Len(t, toolCalls, 1)

	tc := toolCalls[0].(map[string]any)
	assert.Equal(t, "toolu_abc", tc["id"])
	assert.Equal(t, 0, tc["index"])
}

func TestProcessRequest_VertexProvider(t *testing.T) {
	p := NewAPITranslationPlugin()

	cs := newCycleStateWithProvider("vertex")
	req := framework.NewInferenceRequest()
	req.Headers["authorization"] = "Bearer sk-test"
	req.Headers["content-length"] = "200"
	req.Body["model"] = "gemini-2.5-flash"
	req.Body["messages"] = []any{
		map[string]any{"role": "system", "content": "Be concise"},
		map[string]any{"role": "user", "content": "What is Kubernetes?"},
	}
	req.Body["max_tokens"] = float64(100)
	req.Body["temperature"] = 0.7

	err := p.ProcessRequest(context.Background(), cs, req)
	require.NoError(t, err)

	assert.True(t, req.BodyMutated())

	contents, ok := req.Body["contents"].([]map[string]any)
	require.True(t, ok)
	require.Len(t, contents, 1)
	assert.Equal(t, "user", contents[0]["role"])

	sysInstruction, ok := req.Body["systemInstruction"].(map[string]any)
	require.True(t, ok)
	sysParts := sysInstruction["parts"].([]map[string]any)
	assert.Equal(t, "Be concise", sysParts[0]["text"])

	genConfig, ok := req.Body["generationConfig"].(map[string]any)
	require.True(t, ok)
	assert.Equal(t, 100, genConfig["maxOutputTokens"])
	assert.Equal(t, 0.7, genConfig["temperature"])

	mutated := req.MutatedHeaders()
	assert.Equal(t, "/v1beta/models/gemini-2.5-flash:generateContent", mutated[":path"])
	assert.Equal(t, "application/json", mutated["content-type"])

	removed := req.RemovedHeaders()
	assert.Contains(t, removed, "authorization")
	assert.NotContains(t, removed, "content-length")
}

func TestProcessResponse_Vertex(t *testing.T) {
	p := NewAPITranslationPlugin()

	cs := newCycleStateWithProvider("vertex")
	cs.Write(state.ModelKey, "gemini-2.5-flash")

	resp := framework.NewInferenceResponse()
	resp.Body["candidates"] = []any{
		map[string]any{
			"content": map[string]any{
				"parts": []any{map[string]any{"text": "Kubernetes is a container orchestration platform."}},
				"role":  "model",
			},
			"finishReason": "STOP",
		},
	}
	resp.Body["usageMetadata"] = map[string]any{
		"promptTokenCount":     float64(12),
		"candidatesTokenCount": float64(8),
		"totalTokenCount":      float64(20),
	}
	resp.Body["responseId"] = "resp-abc123"

	err := p.ProcessResponse(context.Background(), cs, resp)
	require.NoError(t, err)

	assert.True(t, resp.BodyMutated())
	assert.Equal(t, "chat.completion", resp.Body["object"])
	assert.Equal(t, "gemini-2.5-flash", resp.Body["model"])
	assert.Equal(t, "resp-abc123", resp.Body["id"])

	choices := resp.Body["choices"].([]any)
	choice := choices[0].(map[string]any)
	msg := choice["message"].(map[string]any)
	assert.Equal(t, "assistant", msg["role"])
	assert.Equal(t, "Kubernetes is a container orchestration platform.", msg["content"])
	assert.Equal(t, "stop", choice["finish_reason"])

	usage := resp.Body["usage"].(map[string]any)
	assert.Equal(t, 12, usage["prompt_tokens"])
	assert.Equal(t, 8, usage["completion_tokens"])
	assert.Equal(t, 20, usage["total_tokens"])
}

func TestProcessResponse_VertexError(t *testing.T) {
	p := NewAPITranslationPlugin()

	cs := newCycleStateWithProvider("vertex")

	resp := framework.NewInferenceResponse()
	resp.Body["error"] = map[string]any{
		"code":    float64(400),
		"message": "Invalid value at 'contents'",
		"status":  "INVALID_ARGUMENT",
	}

	err := p.ProcessResponse(context.Background(), cs, resp)
	require.NoError(t, err)

	assert.True(t, resp.BodyMutated())
	errObj := resp.Body["error"].(map[string]any)
	assert.Equal(t, "INVALID_ARGUMENT", errObj["type"])
	assert.Equal(t, "Invalid value at 'contents'", errObj["message"])
}

func TestProcessResponse_VertexToolCall(t *testing.T) {
	p := NewAPITranslationPlugin()

	cs := newCycleStateWithProvider("vertex")
	cs.Write(state.ModelKey, "gemini-2.5-flash")

	resp := framework.NewInferenceResponse()
	resp.Body["candidates"] = []any{
		map[string]any{
			"content": map[string]any{
				"parts": []any{
					map[string]any{
						"functionCall": map[string]any{
							"name": "get_weather",
							"args": map[string]any{"location": "Paris"},
						},
					},
				},
				"role": "model",
			},
			"finishReason": "STOP",
		},
	}
	resp.Body["usageMetadata"] = map[string]any{
		"promptTokenCount":     float64(20),
		"candidatesTokenCount": float64(10),
		"totalTokenCount":      float64(30),
	}

	err := p.ProcessResponse(context.Background(), cs, resp)
	require.NoError(t, err)

	assert.True(t, resp.BodyMutated())

	choices := resp.Body["choices"].([]any)
	choice := choices[0].(map[string]any)
	msg := choice["message"].(map[string]any)
	toolCalls := msg["tool_calls"].([]any)
	require.Len(t, toolCalls, 1)

	tc := toolCalls[0].(map[string]any)
	assert.Equal(t, "call_0", tc["id"])
	assert.Equal(t, 0, tc["index"])
	fn := tc["function"].(map[string]any)
	assert.Equal(t, "get_weather", fn["name"])
}

func TestProcessResponse_NoProviderPassthrough(t *testing.T) {
	p := NewAPITranslationPlugin()

	resp := framework.NewInferenceResponse()
	resp.Body["object"] = "chat.completion"
	resp.Body["choices"] = []any{
		map[string]any{"message": map[string]any{"content": "hi"}},
	}

	err := p.ProcessResponse(context.Background(), framework.NewCycleState(), resp)
	assert.NoError(t, err)
	assert.False(t, resp.BodyMutated())
}

func TestProcessResponse_OpenAIPassthrough(t *testing.T) {
	p := NewAPITranslationPlugin()

	cs := newCycleStateWithProvider("openai")

	resp := framework.NewInferenceResponse()
	resp.Body["object"] = "chat.completion"

	err := p.ProcessResponse(context.Background(), cs, resp)
	assert.NoError(t, err)
	assert.False(t, resp.BodyMutated())
}

func TestFactory_Success(t *testing.T) {
	p, err := APITranslationFactory("test-instance", nil, nil)
	require.NoError(t, err)
	assert.Equal(t, "test-instance", p.TypedName().Name)
	assert.Equal(t, APITranslationPluginType, p.TypedName().Type)
}
