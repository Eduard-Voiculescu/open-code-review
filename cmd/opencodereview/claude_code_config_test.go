// SPDX-License-Identifier: Apache-2.0
// Copyright 2026 alibaba/open-code-review Contributors

package main

import (
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	"github.com/alibaba/open-code-review/internal/claudecli/claudeclitest"
	"github.com/alibaba/open-code-review/internal/llm"
)

func TestMain(m *testing.M) {
	claudeclitest.Main()
	os.Exit(m.Run())
}

func writeClaudeCodeConfig(t *testing.T) string {
	t.Helper()
	home := t.TempDir()
	t.Setenv("HOME", home)
	t.Setenv("USERPROFILE", home)
	dir := filepath.Join(home, ".opencodereview")
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatalf("mkdir config: %v", err)
	}
	path := filepath.Join(dir, "config.json")
	cfg := `{"provider":"claude-code","model":"sonnet","providers":{"claude-code":{}}}`
	if err := os.WriteFile(path, []byte(cfg), 0o600); err != nil {
		t.Fatalf("write config: %v", err)
	}
	return path
}

func TestAWSSettingsRejectedForClaudeCode(t *testing.T) {
	cfg := &Config{}
	err := setProviderValue(cfg, "providers.claude-code.aws_region", "us-west-2")
	if err == nil {
		t.Fatal("aws_region accepted on the claude-code provider; want an error")
	}
	if !strings.Contains(err.Error(), "does not apply to provider") {
		t.Errorf("error = %q, want it to explain the field does not apply", err)
	}
}

func TestScanRefusesClaudeCode(t *testing.T) {
	writeClaudeCodeConfig(t)
	rec := claudeclitest.Install(t, claudeclitest.Script{})
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "a.go"), []byte("package a\n"), 0o644); err != nil {
		t.Fatalf("write a.go: %v", err)
	}

	err := executeScan(scanOptions{repoDir: dir, paths: "a.go", outputFormat: "json"})
	if err == nil || !strings.Contains(err.Error(), "claude-code") {
		t.Fatalf("err = %v, want scan to refuse the claude-code protocol", err)
	}
	if n := len(rec.Invocations(t)); n != 0 {
		t.Errorf("claude invoked %d time(s) before the refusal", n)
	}
}

func TestLLMTestWithClaudeCodeSkipsToolRoundTrip(t *testing.T) {
	path := writeClaudeCodeConfig(t)
	rec := claudeclitest.Install(t, claudeclitest.Script{
		claudeclitest.KindText: {Stdout: claudeclitest.Result("pong", nil, 3, 1)},
	})

	var err error
	out := captureStdout(t, func() { err = runLLMTestWithConfigPath(path) })
	if err != nil {
		t.Fatalf("llm test: %v", err)
	}

	invs := rec.Invocations(t)
	if len(invs) != 1 || invs[0].Kind != claudeclitest.KindText {
		t.Fatalf("invocations = %+v, want one plain text run", invs)
	}
	if slices.Contains(invs[0].Args, "--json-schema") {
		t.Error("llm test forced a tool call through the claude CLI")
	}
	for _, want := range []string{"pong", "Connection test successful", "CLI:"} {
		if !strings.Contains(out, want) {
			t.Errorf("output missing %q:\n%s", want, out)
		}
	}
	if strings.Contains(out, "URL:") {
		t.Errorf("output reports a URL for a CLI backend:\n%s", out)
	}
}

func TestClaudeCodePresetNeedsNoAPIKey(t *testing.T) {
	preset, ok := llm.LookupProvider("claude-code")
	if !ok {
		t.Fatal("claude-code preset missing")
	}
	if err := checkAPIKeyRequirement("claude-code", "", "", preset, true); err != nil {
		t.Errorf("checkAPIKeyRequirement = %v, want nil for the CLI-backed provider", err)
	}
}
