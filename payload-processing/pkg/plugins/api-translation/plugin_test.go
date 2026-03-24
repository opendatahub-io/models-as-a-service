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

// Azure OpenAI and Vertex tests are commented out until their translators are moved to this repo.
// See: TestProcessRequest_AzureOpenAIProvider, TestProcessResponse_AzureOpenAI

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

// Vertex tests are commented out until the Vertex translator is moved to this repo.
// See: TestProcessRequest_VertexProvider, TestProcessResponse_Vertex,
//      TestProcessResponse_VertexError, TestProcessResponse_VertexToolCall

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
