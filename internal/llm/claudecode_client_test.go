// SPDX-License-Identifier: Apache-2.0
// Copyright 2026 alibaba/open-code-review Contributors

package llm_test

import (
	"context"
	"errors"
	"os"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/alibaba/open-code-review/internal/claudecli/claudeclitest"
	"github.com/alibaba/open-code-review/internal/llm"
)

func TestMain(m *testing.M) {
	claudeclitest.Main()
	os.Exit(m.Run())
}

func argValue(args []string, flag string) (string, bool) {
	i := slices.Index(args, flag)
	if i < 0 || i+1 >= len(args) {
		return "", false
	}
	return args[i+1], true
}

func newClaudeCodeClient() llm.LLMClient {
	return llm.NewLLMClient(llm.ResolvedEndpoint{Protocol: llm.ProtocolClaudeCode, Model: "sonnet"}, nil, nil)
}

func TestClaudeCodeClientTextRequest(t *testing.T) {
	rec := claudeclitest.Install(t, claudeclitest.Script{
		claudeclitest.KindText: {Stdout: claudeclitest.Result("grouped", nil, 120, 30)},
	})

	resp, err := newClaudeCodeClient().CompletionsWithCtx(context.Background(), llm.ChatRequest{
		Messages: []llm.Message{
			llm.NewTextMessage("system", "you group files"),
			llm.NewTextMessage("user", "group these"),
		},
	})
	if err != nil {
		t.Fatalf("CompletionsWithCtx: %v", err)
	}
	if got := resp.Content(); got != "grouped" {
		t.Errorf("Content = %q, want grouped", got)
	}
	if resp.Usage == nil || resp.Usage.PromptTokens != 120 || resp.Usage.CompletionTokens != 30 || resp.Usage.TotalTokens != 150 {
		t.Errorf("Usage = %+v, want 120 prompt / 30 completion / 150 total", resp.Usage)
	}

	inv := rec.Invocations(t)[0]
	if inv.Stdin != "group these" {
		t.Errorf("stdin = %q, want the lone user message verbatim", inv.Stdin)
	}
	if got, _ := argValue(inv.Args, "--system-prompt"); got != "you group files" {
		t.Errorf("--system-prompt = %q, want the system message", got)
	}
	if got, _ := argValue(inv.Args, "--model"); got != "sonnet" {
		t.Errorf("--model = %q, want the endpoint model", got)
	}
	if got, _ := argValue(inv.Args, "--max-turns"); got != "1" {
		t.Errorf("--max-turns = %q, want 1 for a text request", got)
	}
}

func TestClaudeCodeClientFlattensConversation(t *testing.T) {
	rec := claudeclitest.Install(t, claudeclitest.Script{
		claudeclitest.KindText: {Stdout: claudeclitest.Result("moved", nil, 1, 1)},
	})

	_, err := newClaudeCodeClient().CompletionsWithCtx(context.Background(), llm.ChatRequest{
		Model: "opus",
		Messages: []llm.Message{
			llm.NewTextMessage("user", "review a.go"),
			llm.NewToolCallMessage("", []llm.ToolCall{{ID: "t1", Function: llm.FunctionCall{Name: "file_read", Arguments: `{"path":"a.go"}`}}}, llm.NativeTurn{}, ""),
			{Role: "tool", ToolCallID: "t1", Content: "package a"},
			llm.NewTextMessage("user", "relocate the comment"),
		},
	})
	if err != nil {
		t.Fatalf("CompletionsWithCtx: %v", err)
	}

	inv := rec.Invocations(t)[0]
	for _, want := range []string{"review a.go", `file_read({"path":"a.go"})`, "package a", "relocate the comment"} {
		if !strings.Contains(inv.Stdin, want) {
			t.Errorf("transcript missing %q:\n%s", want, inv.Stdin)
		}
	}
	if strings.Index(inv.Stdin, "review a.go") > strings.Index(inv.Stdin, "relocate the comment") {
		t.Errorf("transcript out of order:\n%s", inv.Stdin)
	}
	if got, _ := argValue(inv.Args, "--model"); got != "opus" {
		t.Errorf("--model = %q, want the request model to win", got)
	}
}

func TestClaudeCodeClientAnswersToolsAsStructuredOutput(t *testing.T) {
	params := map[string]any{"type": "object", "properties": map[string]any{"comment_ids": map[string]any{"type": "array"}}}
	tools := []llm.ToolDef{
		{Type: "function", Function: llm.FunctionDef{Name: "report_incorrect_comments", Description: "report wrong comments", Parameters: params}},
		{Type: "function", Function: llm.FunctionDef{Name: "approve_all_comments", Description: "keep every comment"}},
	}
	tests := []struct {
		name     string
		tools    []llm.ToolDef
		call     map[string]any
		wantName string
		wantArgs string
	}{
		{
			name:     "single tool",
			tools:    tools[:1],
			call:     map[string]any{"tool": "report_incorrect_comments", "arguments": map[string]any{"comment_ids": []string{"c-1"}}},
			wantName: "report_incorrect_comments",
			wantArgs: `{"comment_ids":["c-1"]}`,
		},
		{
			name:     "choice between tools",
			tools:    tools,
			call:     map[string]any{"tool": "approve_all_comments", "arguments": map[string]any{}},
			wantName: "approve_all_comments",
			wantArgs: `{}`,
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			rec := claudeclitest.Install(t, claudeclitest.Script{
				claudeclitest.KindSchema: {Stdout: claudeclitest.Result("", map[string]any{"call": tt.call}, 5, 5)},
			})

			resp, err := newClaudeCodeClient().CompletionsWithCtx(context.Background(), llm.ChatRequest{
				Messages: []llm.Message{llm.NewTextMessage("user", "filter")},
				Tools:    tt.tools,
			})
			if err != nil {
				t.Fatalf("CompletionsWithCtx: %v", err)
			}

			calls := resp.ToolCalls()
			if len(calls) != 1 || calls[0].Function.Name != tt.wantName || calls[0].Function.Arguments != tt.wantArgs {
				t.Fatalf("ToolCalls = %+v, want one %s call with %s", calls, tt.wantName, tt.wantArgs)
			}
			schema, _ := argValue(rec.Invocations(t)[0].Args, "--json-schema")
			for _, tl := range tt.tools {
				if !strings.Contains(schema, tl.Function.Name) || !strings.Contains(schema, tl.Function.Description) {
					t.Errorf("--json-schema = %s, want %s and its description", schema, tl.Function.Name)
				}
			}
		})
	}
}

func TestClaudeCodeClientRejectsUnofferedTool(t *testing.T) {
	claudeclitest.Install(t, claudeclitest.Script{
		claudeclitest.KindSchema: {Stdout: claudeclitest.Result("", map[string]any{"call": map[string]any{"tool": "rm_rf", "arguments": map[string]any{}}}, 1, 1)},
	})

	_, err := newClaudeCodeClient().CompletionsWithCtx(context.Background(), llm.ChatRequest{
		Messages: []llm.Message{llm.NewTextMessage("user", "filter")},
		Tools:    []llm.ToolDef{{Type: "function", Function: llm.FunctionDef{Name: "approve_all_comments"}}},
	})
	if err == nil || !strings.Contains(err.Error(), "rm_rf") {
		t.Fatalf("err = %v, want a refusal naming the unoffered tool", err)
	}
}

func TestClaudeCodeClientSurfacesCLIErrors(t *testing.T) {
	claudeclitest.Install(t, claudeclitest.Script{
		claudeclitest.KindText: {Stderr: "not logged in", ExitCode: 1},
	})

	_, err := newClaudeCodeClient().CompletionsWithCtx(context.Background(), llm.ChatRequest{
		Messages: []llm.Message{llm.NewTextMessage("user", "hi")},
	})
	if err == nil || !strings.Contains(err.Error(), "not logged in") {
		t.Fatalf("err = %v, want the CLI's stderr", err)
	}
}

func TestClaudeCodeClientAppliesEndpointTimeout(t *testing.T) {
	claudeclitest.Install(t, claudeclitest.Script{claudeclitest.KindText: {Hang: true}})
	client := llm.NewLLMClient(llm.ResolvedEndpoint{Protocol: llm.ProtocolClaudeCode, Timeout: 200 * time.Millisecond}, nil, nil)

	_, err := client.CompletionsWithCtx(context.Background(), llm.ChatRequest{
		Messages: []llm.Message{llm.NewTextMessage("user", "hi")},
	})
	if !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("err = %v, want context.DeadlineExceeded", err)
	}
}
