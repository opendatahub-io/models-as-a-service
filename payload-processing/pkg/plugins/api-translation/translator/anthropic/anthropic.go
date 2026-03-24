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

package anthropic

import (
	"encoding/json"
	"fmt"
	"strings"
	"time"

	"github.com/opendatahub-io/ai-gateway-payload-processing/pkg/plugins/api-translation/translator"
)

const (
	anthropicAPIVersion = "2023-06-01"
	anthropicPath       = "/v1/messages"
	defaultMaxTokens    = 4096
)

// compile-time interface check
var _ translator.Translator = &AnthropicTranslator{}

func NewAnthropicTranslator() *AnthropicTranslator {
	return &AnthropicTranslator{}
}

// AnthropicTranslator translates between OpenAI Chat Completions format and
// Anthropic Messages API format. Works with generic map[string]any bodies.
type AnthropicTranslator struct{}

// TranslateRequest translates an OpenAI Chat Completions request body to Anthropic Messages API format.
func (t *AnthropicTranslator) TranslateRequest(body map[string]any) (map[string]any, map[string]string, []string, error) {
	model, _ := body["model"].(string)
	if model == "" {
		return nil, nil, nil, fmt.Errorf("model field is required")
	}

	messages, err := extractMessages(body)
	if err != nil {
		return nil, nil, nil, fmt.Errorf("failed to extract messages: %w", err)
	}

	systemPrompt, anthropicMessages, err := separateSystemMessages(messages)
	if err != nil {
		return nil, nil, nil, err
	}

	if len(anthropicMessages) == 0 {
		return nil, nil, nil, fmt.Errorf("at least one non-system message is required")
	}

	maxTokens := resolveMaxTokens(body)

	translated := map[string]any{
		"model":      model,
		"messages":   anthropicMessages,
		"max_tokens": maxTokens,
	}

	if systemPrompt != "" {
		translated["system"] = systemPrompt
	}

	if temp, ok := getFloat(body, "temperature"); ok {
		translated["temperature"] = temp
	}
	if topP, ok := getFloat(body, "top_p"); ok {
		translated["top_p"] = topP
	}
	if stop := extractStopSequences(body); len(stop) > 0 {
		translated["stop_sequences"] = stop
	}

	headers := map[string]string{
		"anthropic-version": anthropicAPIVersion,
		"content-type":      "application/json",
		":path":             anthropicPath,
	}

	return translated, headers, nil, nil
}

// TranslateResponse translates an Anthropic Messages API response to OpenAI Chat Completions format.
// Handles both success responses (type: "message") and error responses (type: "error").
func (t *AnthropicTranslator) TranslateResponse(body map[string]any, model string) (map[string]any, error) {
	bodyType, _ := body["type"].(string)

	// Handle Anthropic error responses
	if bodyType == "error" {
		return translateAnthropicError(body), nil
	}

	content := extractAnthropicContent(body)
	finishReason := mapStopReason(body)
	usage := mapAnthropicUsage(body)

	id, _ := body["id"].(string)
	if model == "" {
		model, _ = body["model"].(string)
	}

	message := map[string]any{
		"role":    "assistant",
		"content": content,
	}

	// Extract tool_use blocks if stop_reason is tool_use
	if finishReason == "tool_calls" {
		toolCalls, err := extractAnthropicToolCalls(body)
		if err != nil {
			return nil, fmt.Errorf("failed to extract tool calls: %w", err)
		}
		if len(toolCalls) > 0 {
			message["tool_calls"] = toolCalls
		}
	}

	translated := map[string]any{
		"id":      id,
		"object":  "chat.completion",
		"created": time.Now().Unix(),
		"model":   model,
		"choices": []any{
			map[string]any{
				"index":         0,
				"message":       message,
				"finish_reason": finishReason,
			},
		},
		"usage": usage,
	}

	return translated, nil
}

// translateAnthropicError converts an Anthropic error response to OpenAI error format.
func translateAnthropicError(body map[string]any) map[string]any {
	errObj, _ := body["error"].(map[string]any)
	errType, _ := errObj["type"].(string)
	errMessage, _ := errObj["message"].(string)

	return map[string]any{
		"error": map[string]any{
			"message": errMessage,
			"type":    errType,
			"param":   nil,
			"code":    errType,
		},
	}
}

// separateSystemMessages extracts the system prompt and returns non-system messages
// in Anthropic format (with role and content fields).
// Maps OpenAI "developer" role to Anthropic system prompt (concatenated with "system").
// Returns an error for unsupported roles like "tool" or "function".
func separateSystemMessages(messages []map[string]any) (string, []map[string]any, error) {
	var systemParts []string
	var anthropicMessages []map[string]any

	for i, msg := range messages {
		role, _ := msg["role"].(string)
		content := extractContentString(msg)

		switch role {
		case "system", "developer":
			systemParts = append(systemParts, content)
		case "user":
			anthropicMessages = append(anthropicMessages, map[string]any{
				"role":    "user",
				"content": content,
			})
		case "assistant":
			anthropicMessages = append(anthropicMessages, map[string]any{
				"role":    "assistant",
				"content": content,
			})
		case "tool", "function":
			return "", nil, fmt.Errorf("message at index %d has role '%s' which is not supported for Anthropic translation", i, role)
		default:
			return "", nil, fmt.Errorf("message at index %d has unknown role '%s'", i, role)
		}
	}

	return joinStrings(systemParts, "\n"), anthropicMessages, nil
}

// extractMessages extracts the messages array from the request body.
func extractMessages(body map[string]any) ([]map[string]any, error) {
	rawMessages, ok := body["messages"]
	if !ok {
		return nil, fmt.Errorf("messages field is required")
	}

	messagesSlice, ok := rawMessages.([]any)
	if !ok {
		return nil, fmt.Errorf("messages must be an array")
	}

	var messages []map[string]any
	for i, raw := range messagesSlice {
		msg, ok := raw.(map[string]any)
		if !ok {
			return nil, fmt.Errorf("message at index %d is not an object", i)
		}
		messages = append(messages, msg)
	}

	return messages, nil
}

// extractContentString extracts text content from a message, handling both
// string content and array-of-content-parts formats.
// Only text content is extracted; non-text blocks (image_url, audio, etc.) are skipped.
// Returns empty string if no text content is found.
func extractContentString(msg map[string]any) string {
	content, ok := msg["content"]
	if !ok {
		return ""
	}

	// Simple string content
	if s, ok := content.(string); ok {
		return s
	}

	// Array of content parts (e.g., [{type: "text", text: "hello"}])
	if parts, ok := content.([]any); ok {
		var texts []string
		for _, part := range parts {
			if partMap, ok := part.(map[string]any); ok {
				if text, ok := partMap["text"].(string); ok {
					texts = append(texts, text)
				}
			}
		}
		return joinStrings(texts, " ")
	}

	return ""
}

// resolveMaxTokens extracts max tokens from the request body, checking
// max_completion_tokens first, then max_tokens, defaulting to 4096.
func resolveMaxTokens(body map[string]any) int {
	if v, ok := getInt(body, "max_completion_tokens"); ok && v > 0 {
		return v
	}
	if v, ok := getInt(body, "max_tokens"); ok && v > 0 {
		return v
	}
	return defaultMaxTokens
}

// extractStopSequences extracts stop sequences from the request body,
// handling both string and array formats.
func extractStopSequences(body map[string]any) []string {
	stop, ok := body["stop"]
	if !ok {
		return nil
	}

	if s, ok := stop.(string); ok && s != "" {
		return []string{s}
	}

	if arr, ok := stop.([]any); ok {
		var sequences []string
		for _, v := range arr {
			if s, ok := v.(string); ok {
				sequences = append(sequences, s)
			}
		}
		return sequences
	}

	return nil
}

// extractAnthropicContent extracts text from Anthropic response content blocks.
func extractAnthropicContent(body map[string]any) string {
	contentBlocks, ok := body["content"].([]any)
	if !ok {
		return ""
	}

	var texts []string
	for _, block := range contentBlocks {
		if blockMap, ok := block.(map[string]any); ok {
			if blockType, _ := blockMap["type"].(string); blockType == "text" {
				if text, ok := blockMap["text"].(string); ok {
					texts = append(texts, text)
				}
			}
		}
	}

	return joinStrings(texts, "")
}

// extractAnthropicToolCalls extracts tool_use blocks from an Anthropic response
// and converts them to OpenAI tool_calls format.
func extractAnthropicToolCalls(body map[string]any) ([]any, error) {
	contentBlocks, ok := body["content"].([]any)
	if !ok {
		return nil, nil
	}

	var toolCalls []any
	toolIndex := 0
	for _, block := range contentBlocks {
		blockMap, ok := block.(map[string]any)
		if !ok {
			continue
		}
		if blockType, _ := blockMap["type"].(string); blockType == "tool_use" {
			id, _ := blockMap["id"].(string)
			name, _ := blockMap["name"].(string)
			input := blockMap["input"]

			args, err := toJSONString(input)
			if err != nil {
				return nil, fmt.Errorf("failed to marshal tool call arguments for '%s' - %w", name, err)
			}

			toolCall := map[string]any{
				"id":    id,
				"index": toolIndex,
				"type":  "function",
				"function": map[string]any{
					"name":      name,
					"arguments": args,
				},
			}
			toolCalls = append(toolCalls, toolCall)
			toolIndex++
		}
	}

	return toolCalls, nil
}

// mapStopReason maps Anthropic stop_reason to OpenAI finish_reason.
func mapStopReason(body map[string]any) string {
	reason, _ := body["stop_reason"].(string)
	switch reason {
	case "max_tokens":
		return "length"
	case "tool_use":
		return "tool_calls"
	default:
		return "stop"
	}
}

// mapAnthropicUsage maps Anthropic usage fields to OpenAI format.
func mapAnthropicUsage(body map[string]any) map[string]any {
	usage, ok := body["usage"].(map[string]any)
	if !ok {
		return map[string]any{
			"prompt_tokens":     0,
			"completion_tokens": 0,
			"total_tokens":      0,
		}
	}

	inputTokens := toInt(usage["input_tokens"])
	outputTokens := toInt(usage["output_tokens"])

	return map[string]any{
		"prompt_tokens":     inputTokens,
		"completion_tokens": outputTokens,
		"total_tokens":      inputTokens + outputTokens,
	}
}

// Helper functions for type-safe extraction from map[string]any

func getFloat(body map[string]any, key string) (float64, bool) {
	v, ok := body[key]
	if !ok {
		return 0, false
	}
	switch f := v.(type) {
	case float64:
		return f, true
	case int:
		return float64(f), true
	case int64:
		return float64(f), true
	default:
		return 0, false
	}
}

func getInt(body map[string]any, key string) (int, bool) {
	v, ok := body[key]
	if !ok {
		return 0, false
	}
	switch n := v.(type) {
	case float64:
		return int(n), true
	case int:
		return n, true
	case int64:
		return int(n), true
	default:
		return 0, false
	}
}

func toInt(v any) int {
	switch n := v.(type) {
	case float64:
		return int(n)
	case int:
		return n
	case int64:
		return int(n)
	default:
		return 0
	}
}

func toJSONString(v any) (string, error) {
	if v == nil {
		return "{}", nil
	}
	if s, ok := v.(string); ok {
		return s, nil
	}
	b, err := json.Marshal(v)
	if err != nil {
		return "", fmt.Errorf("failed to marshal to JSON: %w", err)
	}
	return string(b), nil
}

func joinStrings(parts []string, sep string) string {
	return strings.Join(parts, sep)
}
