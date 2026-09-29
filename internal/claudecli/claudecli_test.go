// SPDX-License-Identifier: Apache-2.0
// Copyright 2026 alibaba/open-code-review Contributors

package claudecli_test

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"

	. "github.com/alibaba/open-code-review/internal/claudecli"
	"github.com/alibaba/open-code-review/internal/claudecli/claudeclitest"
)

const (
	successResult = `{"type":"result","subtype":"success","is_error":false,"num_turns":3,"result":"all good",` +
		`"structured_output":{"keep":[1]},"usage":{"input_tokens":100,"output_tokens":20,` +
		`"cache_read_input_tokens":7,"cache_creation_input_tokens":5},"total_cost_usd":0.01}`
	maxTurnsResult = `{"type":"result","subtype":"error_max_turns","is_error":true,"num_turns":4,"usage":{"input_tokens":9,"output_tokens":1}}`
	apiErrorResult = `{"type":"result","subtype":"success","is_error":true,"result":"Invalid API key","usage":{}}`
)

func TestMain(m *testing.M) {
	claudeclitest.Main()
	os.Exit(m.Run())
}

func replyAll(r claudeclitest.Reply) claudeclitest.Script {
	return claudeclitest.Script{claudeclitest.KindText: r, claudeclitest.KindSchema: r, claudeclitest.KindAgent: r}
}

func lastInvocation(t *testing.T, rec *claudeclitest.Recorder) claudeclitest.Invocation {
	t.Helper()
	invs := rec.Invocations(t)
	if len(invs) == 0 {
		t.Fatal("claude was not invoked")
	}
	return invs[len(invs)-1]
}

func flagValue(args []string, flag string) (string, bool) {
	i := slices.Index(args, flag)
	if i < 0 || i+1 >= len(args) {
		return "", false
	}
	return args[i+1], true
}

func TestRunBuildsIsolatedHeadlessInvocation(t *testing.T) {
	rec := claudeclitest.Install(t, replyAll(claudeclitest.Reply{Stdout: successResult}))

	_, err := Run(context.Background(), Request{
		SystemPrompt:  "be a reviewer",
		Prompt:        "review this",
		Model:         "sonnet",
		JSONSchema:    json.RawMessage(`{"type":"object"}`),
		MCPConfigPath: "/tmp/mcp.json",
		AllowedTools:  []string{"mcp__ocr__file_read", "mcp__ocr__code_comment"},
		MaxTurns:      12,
	})
	if err != nil {
		t.Fatalf("Run: %v", err)
	}

	inv := lastInvocation(t, rec)
	if inv.Stdin != "review this" {
		t.Errorf("stdin = %q, want the prompt", inv.Stdin)
	}
	for _, flag := range []string{"-p", "--no-session-persistence", "--strict-mcp-config"} {
		if !slices.Contains(inv.Args, flag) {
			t.Errorf("args %v missing %s", inv.Args, flag)
		}
	}
	want := map[string]string{
		"--output-format":   "json",
		"--tools":           "",
		"--setting-sources": "",
		"--system-prompt":   "be a reviewer",
		"--model":           "sonnet",
		"--json-schema":     `{"type":"object"}`,
		"--mcp-config":      "/tmp/mcp.json",
		"--allowedTools":    "mcp__ocr__file_read,mcp__ocr__code_comment",
		"--max-turns":       "12",
	}
	for flag, value := range want {
		got, ok := flagValue(inv.Args, flag)
		if !ok {
			t.Errorf("args %v missing %s", inv.Args, flag)
			continue
		}
		if got != value {
			t.Errorf("%s = %q, want %q", flag, got, value)
		}
	}
	if repo, _ := os.Getwd(); inv.Dir == repo {
		t.Errorf("claude ran in the caller's directory %q; it must not see project CLAUDE.md or settings", inv.Dir)
	}
}

func TestRunOmitsOptionalFlags(t *testing.T) {
	rec := claudeclitest.Install(t, replyAll(claudeclitest.Reply{Stdout: successResult}))

	if _, err := Run(context.Background(), Request{Prompt: "hi"}); err != nil {
		t.Fatalf("Run: %v", err)
	}

	inv := lastInvocation(t, rec)
	for _, flag := range []string{"--system-prompt", "--model", "--json-schema", "--mcp-config", "--allowedTools", "--max-turns"} {
		if slices.Contains(inv.Args, flag) {
			t.Errorf("args %v contain %s for an empty request field", inv.Args, flag)
		}
	}
}

func TestRunDecodesResult(t *testing.T) {
	claudeclitest.Install(t, replyAll(claudeclitest.Reply{Stdout: successResult}))

	res, err := Run(context.Background(), Request{Prompt: "hi"})
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if res.Text != "all good" {
		t.Errorf("Text = %q, want all good", res.Text)
	}
	if string(res.StructuredOutput) != `{"keep":[1]}` {
		t.Errorf("StructuredOutput = %s, want {\"keep\":[1]}", res.StructuredOutput)
	}
	if res.NumTurns != 3 {
		t.Errorf("NumTurns = %d, want 3", res.NumTurns)
	}
	wantUsage := Usage{InputTokens: 100, OutputTokens: 20, CacheReadInputTokens: 7, CacheCreationInputTokens: 5}
	if res.Usage != wantUsage {
		t.Errorf("Usage = %+v, want %+v", res.Usage, wantUsage)
	}
}

func TestRunFailures(t *testing.T) {
	tests := []struct {
		mode       string
		reply      claudeclitest.Reply
		wantErr    error
		wantResult bool
		contains   string
	}{
		{mode: "max-turns", reply: claudeclitest.Reply{Stdout: maxTurnsResult, ExitCode: 1}, wantErr: ErrMaxTurns, wantResult: true},
		{mode: "api-error", reply: claudeclitest.Reply{Stdout: apiErrorResult, ExitCode: 1}, contains: "Invalid API key"},
		{mode: "crash", reply: claudeclitest.Reply{Stderr: "boom: not logged in\n", ExitCode: 2}, contains: "boom: not logged in"},
		{mode: "garbage", reply: claudeclitest.Reply{Stdout: "not json"}, contains: "decode"},
	}
	for _, tt := range tests {
		t.Run(tt.mode, func(t *testing.T) {
			claudeclitest.Install(t, replyAll(tt.reply))

			res, err := Run(context.Background(), Request{Prompt: "hi"})
			if err == nil {
				t.Fatal("Run succeeded; want an error")
			}
			if tt.wantErr != nil && !errors.Is(err, tt.wantErr) {
				t.Errorf("err = %v, want %v", err, tt.wantErr)
			}
			if tt.contains != "" && !strings.Contains(err.Error(), tt.contains) {
				t.Errorf("err = %q, want it to contain %q", err, tt.contains)
			}
			if tt.wantResult && (res == nil || res.Usage.InputTokens != 9) {
				t.Errorf("result = %+v, want the decoded result alongside the error", res)
			}
		})
	}
}

func TestRunMissingBinary(t *testing.T) {
	t.Setenv(BinaryEnv, filepath.Join(t.TempDir(), "no-such-claude"))

	_, err := Run(context.Background(), Request{Prompt: "hi"})
	if err == nil || !strings.Contains(err.Error(), "Claude Code") {
		t.Fatalf("err = %v, want a hint to install Claude Code", err)
	}
}

func TestRunHonoursContext(t *testing.T) {
	claudeclitest.Install(t, replyAll(claudeclitest.Reply{Hang: true}))
	ctx, cancel := context.WithTimeout(context.Background(), 200*time.Millisecond)
	defer cancel()

	start := time.Now()
	_, err := Run(ctx, Request{Prompt: "hi"})
	if !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("err = %v, want context.DeadlineExceeded", err)
	}
	if elapsed := time.Since(start); elapsed > 10*time.Second {
		t.Errorf("Run took %s after the deadline", elapsed)
	}
}

func TestBinaryDefaultsToClaude(t *testing.T) {
	t.Setenv(BinaryEnv, "")
	if got := Binary(); got != "claude" {
		t.Errorf("Binary() = %q, want claude", got)
	}
	t.Setenv(BinaryEnv, " /opt/claude ")
	if got := Binary(); got != "/opt/claude" {
		t.Errorf("Binary() = %q, want /opt/claude", got)
	}
}

func TestRunRaisesMCPOutputCapForToolRuns(t *testing.T) {
	tests := []struct {
		name    string
		mcp     string
		userCap string
		want    string
	}{
		{name: "tool run", mcp: "/tmp/mcp.json", want: MCPOutputTokens},
		{name: "user cap wins", mcp: "/tmp/mcp.json", userCap: "5000", want: "5000"},
		{name: "text run untouched", want: ""},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Setenv("MAX_MCP_OUTPUT_TOKENS", tt.userCap)
			rec := claudeclitest.Install(t, replyAll(claudeclitest.Reply{Stdout: successResult}))

			if _, err := Run(context.Background(), Request{Prompt: "hi", MCPConfigPath: tt.mcp}); err != nil {
				t.Fatalf("Run: %v", err)
			}
			if got := lastInvocation(t, rec).MCPOutputTokens; got != tt.want {
				t.Errorf("MAX_MCP_OUTPUT_TOKENS = %q, want %q", got, tt.want)
			}
		})
	}
}
