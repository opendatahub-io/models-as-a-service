/*
Copyright 2025.

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

package maas

import (
	"testing"
)

func TestValidateVirtualName(t *testing.T) {
	tests := []struct {
		name     string
		input    string
		expected bool
	}{
		{"valid name", "claude", true},
		{"valid with numbers", "gpt4", true},
		{"valid with dash", "claude-3", true},
		{"valid with underscore", "model_v2", true},
		{"valid with dot", "model.v1", true},
		{"valid complex", "claude-3.5-sonnet", true},
		{"empty string", "", false},
		{"starts with dash", "-invalid", false},
		{"starts with dot", ".invalid", false},
		{"starts with underscore", "_invalid", false},
		{"contains space", "claude 3", false},
		{"contains slash", "claude/3", false},
		{"contains special chars", "claude@anthropic", false},
		{"only numbers", "123", true},
		{"only dashes", "---", false},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			result := validateVirtualName(tt.input)
			if result != tt.expected {
				t.Errorf("validateVirtualName(%q) = %v, want %v", tt.input, result, tt.expected)
			}
		})
	}
}

func TestValidateBackendModelName(t *testing.T) {
	tests := []struct {
		name     string
		input    string
		expected bool
	}{
		{"valid backend name", "claude-3-5-sonnet-20241022", true},
		{"valid openai name", "gpt-4o", true},
		{"valid llama name", "llama-7b-instruct", true},
		{"empty string", "", false},
		{"starts with special char", "-model", false},
		{"contains invalid chars", "model@provider", false},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			result := validateBackendModelName(tt.input)
			if result != tt.expected {
				t.Errorf("validateBackendModelName(%q) = %v, want %v", tt.input, result, tt.expected)
			}
		})
	}
}

func TestValidateModelAliases(t *testing.T) {
	tests := []struct {
		name     string
		input    []string
		expected []string
	}{
		{
			name:     "valid aliases",
			input:    []string{"claude", "claude-3", "anthropic"},
			expected: []string{"claude", "claude-3", "anthropic"},
		},
		{
			name:     "with invalid aliases",
			input:    []string{"claude", "invalid@name", "claude-3"},
			expected: []string{"claude", "claude-3"},
		},
		{
			name:     "with duplicates",
			input:    []string{"claude", "claude", "claude-3"},
			expected: []string{"claude", "claude-3"},
		},
		{
			name:     "with empty strings",
			input:    []string{"claude", "", "claude-3"},
			expected: []string{"claude", "claude-3"},
		},
		{
			name:     "with whitespace",
			input:    []string{"  claude  ", "claude-3", "  "},
			expected: []string{"claude", "claude-3"},
		},
		{
			name:     "empty input",
			input:    []string{},
			expected: []string{},
		},
		{
			name:     "all invalid",
			input:    []string{"@invalid", "-bad", ""},
			expected: []string{},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			result := validateModelAliases(tt.input)
			if len(result) != len(tt.expected) {
				t.Errorf("validateModelAliases(%v) returned %d items, want %d", tt.input, len(result), len(tt.expected))
				return
			}
			for i, expected := range tt.expected {
				if result[i] != expected {
					t.Errorf("validateModelAliases(%v)[%d] = %q, want %q", tt.input, i, result[i], expected)
				}
			}
		})
	}
}