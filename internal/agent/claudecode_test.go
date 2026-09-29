// SPDX-License-Identifier: Apache-2.0
// Copyright 2026 alibaba/open-code-review Contributors

package agent

import (
	"context"
	"strings"
	"testing"

	"github.com/alibaba/open-code-review/internal/claudecli/claudeclitest"
	"github.com/alibaba/open-code-review/internal/config/template"
	"github.com/alibaba/open-code-review/internal/llm"
	"github.com/alibaba/open-code-review/internal/model"
	"github.com/alibaba/open-code-review/internal/session"
	"github.com/alibaba/open-code-review/internal/tool"
)

func TestClaudeCodeProtocolDrivesMainTaskOverMCP(t *testing.T) {
	rec := claudeclitest.Install(t, claudeclitest.Script{claudeclitest.KindAgent: {
		ToolCalls: []claudeclitest.ToolCall{
			{Name: "code_comment", Args: map[string]any{"comments": []any{
				map[string]any{"path": "calc.go", "content": "division by zero is unchecked", "existing_code": "return a / b"},
			}}},
			{Name: "task_done", Args: map[string]any{}},
		},
		Stdout: claudeclitest.Result("reviewed", nil, 900, 80),
	}})

	setTestHome(t, t.TempDir())
	repoDir := t.TempDir()
	sh := session.New(repoDir, "feature", "sonnet", session.SessionOptions{
		ReviewMode: session.ReviewModeRange, DiffFrom: "main", DiffTo: "feature", Operation: session.OperationReview,
	})
	t.Cleanup(func() { _ = sh.Finalize() })
	collector := tool.NewCommentCollector()
	reg := tool.NewRegistry()
	reg.Register(&tool.CodeCommentProvider{Collector: collector})
	reg.Freeze()
	ep := llm.ResolvedEndpoint{Protocol: llm.ProtocolClaudeCode, Model: "sonnet"}

	a := New(Args{
		RepoDir:          repoDir,
		From:             "main",
		To:               "feature",
		ReviewMode:       session.ReviewModeRange,
		LLMClient:        llm.NewLLMClient(ep, nil, nil),
		Model:            "sonnet",
		Session:          sh,
		Tools:            reg,
		CommentCollector: collector,
		RuntimeConfig:    RuntimeConfig{Protocol: llm.ProtocolClaudeCode},
		Template: template.Template{
			MaxTokens:           100000,
			MaxToolRequestTimes: 9,
			MainTask: template.LlmConversation{
				Messages: []template.ChatMessage{{Role: "user", Content: "Review {{diffs}}"}},
			},
		},
		MainToolDefs: []llm.ToolDef{
			{Type: "function", Function: llm.FunctionDef{Name: "code_comment", Parameters: map[string]any{"type": "object"}}},
			{Type: "function", Function: llm.FunctionDef{Name: "task_done"}},
		},
	})
	a.diffs = []model.Diff{{OldPath: "calc.go", NewPath: "calc.go", Diff: "@@ -1,1 +1,1 @@\n+return a / b", Insertions: 1}}
	a.currentDate = "2026-09-28 12:00"

	comments, err := a.dispatchSubtasks(context.Background())
	if err != nil {
		t.Fatalf("dispatch: %v", err)
	}

	invs := rec.Invocations(t)
	if len(invs) != 1 || invs[0].Kind != claudeclitest.KindAgent {
		t.Fatalf("invocations = %+v, want one agent run", invs)
	}
	if invs[0].ToolError != "" {
		t.Fatalf("fake claude tool error: %s", invs[0].ToolError)
	}
	if !strings.Contains(invs[0].Stdin, "return a / b") {
		t.Errorf("prompt does not carry the diff:\n%s", invs[0].Stdin)
	}
	if len(comments) != 1 || comments[0].Path != "calc.go" || !strings.Contains(comments[0].Content, "division by zero") {
		t.Fatalf("comments = %+v, want the one filed over MCP", comments)
	}
	if got := a.TotalInputTokens(); got != 900 {
		t.Errorf("TotalInputTokens = %d, want 900 from the claude result", got)
	}
	if got := a.ToolCalls()["code_comment"]; got != 1 {
		t.Errorf("ToolCalls[code_comment] = %d, want 1", got)
	}
	manifest := finishManifestFlow(t, a)
	if manifest.TerminalState != session.StateComplete || len(manifest.Coverage.Completed) != 1 {
		t.Fatalf("manifest = %+v, want a complete single-item run", manifest)
	}
}
