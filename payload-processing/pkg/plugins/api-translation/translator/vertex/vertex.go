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

package vertex

import (
	"encoding/json"
	"fmt"
	"regexp"
	"strings"
	"time"
)

const (
	vertexV1BetaPathTemplate = "/v1beta/models/%s:generateContent"
)

// compile-time interface check
// var _ translator.Translator = &VertexTranslator{}

func NewVertexTranslator() *VertexTranslator {
	return &VertexTranslator{
		modelNamePattern: regexp.MustCompile(`^[a-zA-Z0-9][a-zA-Z0-9._-]*$`), // modelNamePattern validates Gemini model names to prevent path injection.
	}
}

// VertexTranslator translates between OpenAI Chat Completions format and
// Google Vertex AI (Gemini) GenerateContent API format.
type VertexTranslator struct {
	modelNamePattern *regexp.Regexp
}

// TranslateRequest translates an OpenAI Chat Completions request body to
// Vertex AI GenerateContent API format.
func (t *VertexTranslator) TranslateRequest(body map[string]any) (map[string]any, map[string]string, []string, error) {
	model, _ := body["model"].(string)
	if model == "" {
		return nil, nil, nil, fmt.Errorf("model field is required")
	}

	if !t.modelNamePattern.MatchString(model) {
		return nil, nil, nil, fmt.Errorf("model '%s' contains invalid characters for Vertex AI model name", model)
	}

	messages, err := extractMessages(body)
	if err != nil {
		return nil, nil, nil, fmt.Errorf("failed to extract messages: %w", err)
	}

	systemParts, contents, err := separateSystemMessages(messages)
	if err != nil {
		return nil, nil, nil, err
	}

	if len(contents) == 0 {
		return nil, nil, nil, fmt.Errorf("at least one non-system message is required")
	}

	translated := map[string]any{
		"contents": contents,
	}

	if len(systemParts) > 0 {
		translated["systemInstruction"] = map[string]any{
			"parts": systemParts,
		}
	}

	generationConfig := buildGenerationConfig(body)
	if len(generationConfig) > 0 {
		translated["generationConfig"] = generationConfig
	}

	// Use v1beta for systemInstruction support on the Generative Language API.
	// The Vertex AI API (aiplatform.googleapis.com) supports systemInstruction in v1,
	// but the Generative Language API (generativelanguage.googleapis.com) requires v1beta.
	// Using v1beta is compatible with both endpoints.
	path := fmt.Sprintf(vertexV1BetaPathTemplate, model)

	headers := map[string]string{
		"content-type": "application/json",
		":path":        path,
	}

	return translated, headers, nil, nil
}

// TranslateResponse translates a Vertex AI GenerateContent response to
// OpenAI Chat Completions format. Handles both success and error responses.
func (t *VertexTranslator) TranslateResponse(body map[string]any, model string) (map[string]any, error) {
	if errObj, ok := body["error"].(map[string]any); ok {
		return translateVertexError(errObj), nil
	}

	choices := translateCandidates(body)

	usage := mapVertexUsage(body)

	if model == "" {
		if mv, ok := body["modelVersion"].(string); ok && mv != "" {
			model = mv
		}
	}

	responseID, _ := body["responseId"].(string)

	translated := map[string]any{
		"id":      responseID,
		"object":  "chat.completion",
		"created": time.Now().Unix(),
		"model":   model,
		"choices": choices,
		"usage":   usage,
	}

	return translated, nil
}

// separateSystemMessages separates system/developer messages into systemInstruction
// parts and converts the remaining messages into Vertex contents format.
func separateSystemMessages(messages []map[string]any) ([]map[string]any, []map[string]any, error) {
	var systemParts []map[string]any
	var contents []map[string]any

	for i, msg := range messages {
		role, _ := msg["role"].(string)
		content := extractContentString(msg)

		switch role {
		case "system", "developer":
			systemParts = append(systemParts, map[string]any{"text": content})
		case "user":
			contents = append(contents, map[string]any{
				"role":  "user",
				"parts": []map[string]any{{"text": content}},
			})
		case "assistant":
			contents = append(contents, map[string]any{
				"role":  "model",
				"parts": []map[string]any{{"text": content}},
			})
		case "tool", "function":
			return nil, nil, fmt.Errorf("message at index %d has role '%s' which is not supported for Vertex translation", i, role)
		default:
			return nil, nil, fmt.Errorf("message at index %d has unknown role '%s'", i, role)
		}
	}

	return systemParts, contents, nil
}

// buildGenerationConfig constructs the Vertex generationConfig from OpenAI parameters.
func buildGenerationConfig(body map[string]any) map[string]any {
	config := map[string]any{}

	maxTokens := resolveMaxTokens(body)
	if maxTokens > 0 {
		config["maxOutputTokens"] = maxTokens
	}

	if temp, ok := getFloat(body, "temperature"); ok {
		config["temperature"] = temp
	}
	if topP, ok := getFloat(body, "top_p"); ok {
		config["topP"] = topP
	}
	if stop := extractStopSequences(body); len(stop) > 0 {
		config["stopSequences"] = stop
	}

	return config
}

// translateCandidates converts Vertex candidates to OpenAI choices.
func translateCandidates(body map[string]any) []any {
	candidatesRaw, ok := body["candidates"].([]any)
	if !ok || len(candidatesRaw) == 0 {
		return []any{}
	}

	var choices []any
	for i, raw := range candidatesRaw {
		candidate, ok := raw.(map[string]any)
		if !ok {
			continue
		}

		text, toolCalls := extractCandidateContent(candidate)
		finishReason := mapFinishReason(candidate)

		message := map[string]any{
			"role":    "assistant",
			"content": text,
		}

		if len(toolCalls) > 0 {
			message["tool_calls"] = toolCalls
		}

		choice := map[string]any{
			"index":         i,
			"message":       message,
			"finish_reason": finishReason,
		}

		choices = append(choices, choice)
	}

	return choices
}

// extractCandidateContent extracts text content and tool calls from a Vertex candidate.
func extractCandidateContent(candidate map[string]any) (string, []any) {
	content, ok := candidate["content"].(map[string]any)
	if !ok {
		return "", nil
	}

	partsRaw, ok := content["parts"].([]any)
	if !ok {
		return "", nil
	}

	var texts []string
	var toolCalls []any
	toolIndex := 0

	for _, raw := range partsRaw {
		part, ok := raw.(map[string]any)
		if !ok {
			continue
		}

		if text, ok := part["text"].(string); ok {
			texts = append(texts, text)
		}

		if fc, ok := part["functionCall"].(map[string]any); ok {
			name, _ := fc["name"].(string)
			args := fc["args"]

			argsStr, err := toJSONString(args)
			if err != nil {
				continue
			}

			toolCall := map[string]any{
				"id":    fmt.Sprintf("call_%d", toolIndex),
				"index": toolIndex,
				"type":  "function",
				"function": map[string]any{
					"name":      name,
					"arguments": argsStr,
				},
			}
			toolCalls = append(toolCalls, toolCall)
			toolIndex++
		}
	}

	return strings.Join(texts, ""), toolCalls
}

// mapFinishReason maps Vertex finishReason to OpenAI finish_reason.
func mapFinishReason(candidate map[string]any) string {
	reason, _ := candidate["finishReason"].(string)
	switch reason {
	case "STOP":
		return "stop"
	case "MAX_TOKENS":
		return "length"
	case "SAFETY", "RECITATION", "BLOCKLIST", "PROHIBITED_CONTENT", "SPII",
		"MODEL_ARMOR", "IMAGE_SAFETY", "IMAGE_PROHIBITED_CONTENT", "IMAGE_RECITATION":
		return "content_filter"
	case "MALFORMED_FUNCTION_CALL", "UNEXPECTED_TOOL_CALL":
		return "tool_calls"
	default:
		return "stop"
	}
}

// mapVertexUsage maps Vertex usageMetadata to OpenAI usage format.
func mapVertexUsage(body map[string]any) map[string]any {
	usage, ok := body["usageMetadata"].(map[string]any)
	if !ok {
		return map[string]any{
			"prompt_tokens":     0,
			"completion_tokens": 0,
			"total_tokens":      0,
		}
	}

	promptTokens := toInt(usage["promptTokenCount"])
	completionTokens := toInt(usage["candidatesTokenCount"])
	totalTokens := toInt(usage["totalTokenCount"])

	return map[string]any{
		"prompt_tokens":     promptTokens,
		"completion_tokens": completionTokens,
		"total_tokens":      totalTokens,
	}
}

// translateVertexError converts a Vertex error object to OpenAI error format.
func translateVertexError(errObj map[string]any) map[string]any {
	message, _ := errObj["message"].(string)
	status, _ := errObj["status"].(string)
	code := errObj["code"]

	codeStr := ""
	if code != nil {
		codeStr = fmt.Sprintf("%v", code)
	}

	return map[string]any{
		"error": map[string]any{
			"message": message,
			"type":    status,
			"param":   nil,
			"code":    codeStr,
		},
	}
}

// --- Helper functions ---

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

func extractContentString(msg map[string]any) string {
	content, ok := msg["content"]
	if !ok {
		return ""
	}

	if s, ok := content.(string); ok {
		return s
	}

	if parts, ok := content.([]any); ok {
		var texts []string
		for _, part := range parts {
			if partMap, ok := part.(map[string]any); ok {
				if text, ok := partMap["text"].(string); ok {
					texts = append(texts, text)
				}
			}
		}
		return strings.Join(texts, " ")
	}

	return ""
}

func resolveMaxTokens(body map[string]any) int {
	if v, ok := getInt(body, "max_completion_tokens"); ok && v > 0 {
		return v
	}
	if v, ok := getInt(body, "max_tokens"); ok && v > 0 {
		return v
	}
	return 0
}

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
