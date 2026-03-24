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

package awsbedrock

import (
	"fmt"
	"os"
)

const (
	// AWS environment variables
	awsRegionEnv     = "AWS_REGION"
	awsDefaultRegion = "us-east-1"

	// Bedrock OpenAI-compatible endpoint path
	bedrockOpenAIPath = "/v1/chat/completions"
)

// BedrockTranslator translates OpenAI Chat Completions to AWS Bedrock's OpenAI-compatible API
// This is a simple path rewriter since Bedrock's OpenAI-compatible endpoint uses the same format
type BedrockTranslator struct {
	region string
}

// NewBedrockTranslator creates a new AWS Bedrock translator instance
func NewBedrockTranslator() *BedrockTranslator {
	region := os.Getenv(awsRegionEnv)
	if region == "" {
		region = awsDefaultRegion
	}

	return &BedrockTranslator{
		region: region,
	}
}

// NewBedrockTranslatorWithRegion creates a translator with explicit region
func NewBedrockTranslatorWithRegion(region string) *BedrockTranslator {
	if region == "" {
		region = awsDefaultRegion
	}

	return &BedrockTranslator{
		region: region,
	}
}

// TranslateRequest translates an OpenAI request to AWS Bedrock Converse API format
// Returns the translated body, headers to set, headers to remove, and any error
// TranslateRequest rewrites the path to target Bedrock's OpenAI-compatible endpoint.
// The request body is not mutated since Bedrock's OpenAI-compatible API accepts the same schema as OpenAI.
func (t *BedrockTranslator) TranslateRequest(body map[string]any) (
	translatedBody map[string]any,
	headersToMutate map[string]string,
	headersToRemove []string,
	err error,
) {
	// Validate required fields
	model, ok := body["model"].(string)
	if !ok || model == "" {
		return nil, nil, nil, fmt.Errorf("model field is required")
	}

	// Validate this is a Chat Completions request
	if _, hasMessages := body["messages"]; !hasMessages {
		return nil, nil, nil, fmt.Errorf("only Chat Completions API is supported - 'messages' field required")
	}

	// Build headers for Bedrock OpenAI-compatible endpoint
	headersToMutate = map[string]string{
		":path":        bedrockOpenAIPath,
		"content-type": "application/json",
		"host":         fmt.Sprintf("bedrock-runtime.%s.amazonaws.com", t.region),
	}

	// Return nil body — no body mutation needed, Bedrock accepts OpenAI request format as-is
	return nil, headersToMutate, nil, nil
}

// TranslateResponse is a no-op since Bedrock's OpenAI-compatible API returns responses in OpenAI format
func (t *BedrockTranslator) TranslateResponse(body map[string]any, model string) (
	translatedBody map[string]any,
	err error,
) {
	// No translation needed - Bedrock's OpenAI-compatible endpoint returns OpenAI format
	return nil, nil
}

// GetRegion returns the AWS region configured for this translator
func (t *BedrockTranslator) GetRegion() string {
	return t.region
}