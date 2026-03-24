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

package translator

// Translator translates between OpenAI Chat Completions format and a specific
// provider's native API format. Implementations handle both request and response
// translation using generic map[string]any bodies (pre-parsed JSON from the BBR framework).
type Translator interface {
	// TranslateRequest translates an OpenAI-format request body to the provider's native format.
	// Returns the translated body, headers to set, headers to remove, and any error.
	// A nil translatedBody means no body mutation is needed.
	TranslateRequest(body map[string]any) (translatedBody map[string]any, headersToMutate map[string]string, headersToRemove []string, err error)

	// TranslateResponse translates a provider-native response body back to OpenAI Chat Completions format.
	// The model parameter is used to populate the model field in the OpenAI response.
	// A nil translatedBody means no body mutation is needed.
	TranslateResponse(body map[string]any, model string) (translatedBody map[string]any, err error)
}
