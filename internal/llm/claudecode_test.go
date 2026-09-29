// SPDX-License-Identifier: Apache-2.0
// Copyright 2026 alibaba/open-code-review Contributors

package llm

import "testing"

func TestClaudeCodeProtocolIsRecognized(t *testing.T) {
	for _, raw := range []string{"claude-code", "CLAUDE-CODE", "  Claude-Code  "} {
		if got := NormalizeProtocol(raw); got != ProtocolClaudeCode {
			t.Errorf("NormalizeProtocol(%q) = %q, want %q", raw, got, ProtocolClaudeCode)
		}
	}
	if err := ValidateProtocol(ProtocolClaudeCode); err != nil {
		t.Errorf("ValidateProtocol(%q) = %v, want nil", ProtocolClaudeCode, err)
	}
}

func TestClaudeCodeProviderIsRegistered(t *testing.T) {
	p, ok := LookupProvider("claude-code")
	if !ok {
		t.Fatal("LookupProvider(\"claude-code\") not found")
	}
	if p.Protocol != ProtocolClaudeCode {
		t.Errorf("Protocol = %q, want %q", p.Protocol, ProtocolClaudeCode)
	}
	if !p.AmbientAuth {
		t.Error("AmbientAuth = false, want true — the claude CLI owns the login")
	}
	if p.BaseURL != "" || p.EnvVar != "" {
		t.Errorf("BaseURL/EnvVar = %q/%q, want both empty", p.BaseURL, p.EnvVar)
	}
}

func TestResolveClaudeCodeWithoutAPIKey(t *testing.T) {
	tests := []struct {
		name string
		cfg  map[string]any
	}{
		{
			name: "preset",
			cfg: map[string]any{
				"provider":  "claude-code",
				"model":     "sonnet",
				"providers": map[string]any{"claude-code": map[string]any{}},
			},
		},
		{
			name: "preset without a providers entry",
			cfg:  map[string]any{"provider": "claude-code", "model": "sonnet"},
		},
		{
			name: "custom provider without url",
			cfg: map[string]any{
				"provider": "mine",
				"model":    "sonnet",
				"custom_providers": map[string]any{
					"mine": map[string]any{"protocol": ProtocolClaudeCode},
				},
			},
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			ep, err := ResolveEndpoint(writeConfig(t, tt.cfg))
			if err != nil {
				t.Fatalf("ResolveEndpoint: %v", err)
			}
			if ep.Protocol != ProtocolClaudeCode {
				t.Errorf("Protocol = %q, want %q", ep.Protocol, ProtocolClaudeCode)
			}
			if !ep.AmbientAuth {
				t.Error("AmbientAuth = false, want true")
			}
			if ep.Token != "" {
				t.Errorf("Token = %q, want empty", ep.Token)
			}
			if ep.Model != "sonnet" {
				t.Errorf("Model = %q, want sonnet", ep.Model)
			}
		})
	}
}
