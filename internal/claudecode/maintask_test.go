// SPDX-License-Identifier: Apache-2.0
// Copyright 2026 alibaba/open-code-review Contributors

package claudecode

import (
	"context"
	"os"
	"slices"
	"strings"
	"testing"

	"github.com/alibaba/open-code-review/internal/claudecli/claudeclitest"
	"github.com/alibaba/open-code-review/internal/llm"
	"github.com/alibaba/open-code-review/internal/llmloop"
	"github.com/alibaba/open-code-review/internal/session"
)

func TestMain(m *testing.M) {
	claudeclitest.Main()
	os.Exit(m.Run())
}

type recordingRunner struct {
	recordingExecutor
	usage []*llm.UsageInfo
	spent int64
}

func (r *recordingRunner) RecordUsage(u *llm.UsageInfo) { r.usage = append(r.usage, u) }

func (r *recordingRunner) TotalTokensUsed() int64 { return r.spent }

func newMainTask(t *testing.T, runner *recordingRunner) (*MainTask, *session.SessionHistory) {
	t.Helper()
	sess := session.New(t.TempDir(), "main", "sonnet", session.SessionOptions{ReviewMode: "diff"})
	return NewMainTask(MainTaskDeps{
		Runner:   runner,
		Session:  sess,
		ToolDefs: testToolDefs,
		Model:    "sonnet",
		MaxTurns: 7,
	}), sess
}

var mainMessages = []llm.Message{
	llm.NewTextMessage("system", "you review code"),
	llm.NewTextMessage("user", "review group-a"),
}

func argAfter(args []string, flag string) string {
	if i := slices.Index(args, flag); i >= 0 && i+1 < len(args) {
		return args[i+1]
	}
	return ""
}

func TestMainTaskServesToolsToClaude(t *testing.T) {
	rec := claudeclitest.Install(t, claudeclitest.Script{claudeclitest.KindAgent: {
		ToolCalls: []claudeclitest.ToolCall{
			{Name: "file_read", Args: map[string]any{"path": "a.go"}},
			{Name: "task_done", Args: map[string]any{}},
		},
		Stdout: claudeclitest.Result("done", nil, 300, 40),
	}})
	runner := &recordingRunner{}
	task, sess := newMainTask(t, runner)

	completed, stop, err := task.RunMainTask(context.Background(), mainMessages, "group-a")
	if err != nil {
		t.Fatalf("RunMainTask: %v", err)
	}
	if !completed || stop != llmloop.StopNone {
		t.Errorf("completed/stop = %v/%v, want true/StopNone", completed, stop)
	}

	inv := rec.Invocations(t)[0]
	if inv.ToolError != "" {
		t.Fatalf("fake claude tool error: %s", inv.ToolError)
	}
	if len(inv.ToolResults) != 2 || inv.ToolResults[0] != `read {"path":"a.go"}` {
		t.Errorf("tool results = %q, want the executor's file_read output first", inv.ToolResults)
	}
	if runner.keys[0] != "group-a" {
		t.Errorf("task key = %q, want group-a", runner.keys[0])
	}
	if inv.Stdin != "review group-a" {
		t.Errorf("stdin = %q, want the user message", inv.Stdin)
	}
	if sys := argAfter(inv.Args, "--system-prompt"); !strings.HasPrefix(sys, "you review code") {
		t.Errorf("--system-prompt = %q, want the template's system message first", sys)
	}
	if got := argAfter(inv.Args, "--allowedTools"); !strings.Contains(got, "mcp__ocr__file_read") {
		t.Errorf("--allowedTools = %q, want the ocr MCP tools", got)
	}
	if got := argAfter(inv.Args, "--max-turns"); got != "7" {
		t.Errorf("--max-turns = %q, want 7", got)
	}
	if len(runner.usage) != 1 || runner.usage[0].PromptTokens != 300 || runner.usage[0].CompletionTokens != 40 {
		t.Errorf("usage = %+v, want one 300/40 record", runner.usage)
	}
	records := sess.GetOrCreateFileSession("group-a").TaskRecords[session.MainTask]
	if len(records) != 1 || records[0].Response == nil {
		t.Fatalf("main-task records = %+v, want one record with the response", records)
	}
}

func TestMainTaskOutcomes(t *testing.T) {
	maxTurns := `{"type":"result","subtype":"error_max_turns","is_error":true,"num_turns":7,"usage":{"input_tokens":5,"output_tokens":1}}`
	tests := []struct {
		name          string
		reply         claudeclitest.Reply
		wantCompleted bool
		wantStop      llmloop.MainLoopStop
		wantErr       string
		wantUsage     bool
	}{
		{
			name: "ends without task_done after using tools",
			reply: claudeclitest.Reply{
				ToolCalls: []claudeclitest.ToolCall{{Name: "file_read", Args: map[string]any{"path": "a.go"}}},
				Stdout:    claudeclitest.Result("nothing to flag", nil, 1, 1),
			},
			wantCompleted: true,
			wantUsage:     true,
		},
		{
			name:      "ends without ever calling a tool",
			reply:     claudeclitest.Reply{Stdout: claudeclitest.Result("I could not reach any tools", nil, 1, 1)},
			wantErr:   "without calling any ocr tool",
			wantUsage: true,
		},
		{
			name:      "turn limit",
			reply:     claudeclitest.Reply{Stdout: maxTurns, ExitCode: 1},
			wantStop:  llmloop.StopMaxRounds,
			wantUsage: true,
		},
		{
			name: "task_done after turn limit still completes",
			reply: claudeclitest.Reply{
				ToolCalls: []claudeclitest.ToolCall{{Name: "task_done", Args: map[string]any{}}},
				Stdout:    maxTurns,
				ExitCode:  1,
			},
			wantCompleted: true,
			wantUsage:     true,
		},
		{
			name: "task_done FAILED",
			reply: claudeclitest.Reply{
				ToolCalls: []claudeclitest.ToolCall{{Name: "explode", Args: map[string]any{}}},
				Stdout:    claudeclitest.Result("", nil, 1, 1),
			},
			wantErr:   "task failed: boom",
			wantUsage: true,
		},
		{
			name:    "cli error",
			reply:   claudeclitest.Reply{Stderr: "not logged in", ExitCode: 1},
			wantErr: "not logged in",
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			claudeclitest.Install(t, claudeclitest.Script{claudeclitest.KindAgent: tt.reply})
			runner := &recordingRunner{}
			task, _ := newMainTask(t, runner)

			completed, stop, err := task.RunMainTask(context.Background(), mainMessages, "g")
			if tt.wantErr != "" {
				if err == nil || !strings.Contains(err.Error(), tt.wantErr) {
					t.Fatalf("err = %v, want %q", err, tt.wantErr)
				}
			} else if err != nil {
				t.Fatalf("RunMainTask: %v", err)
			}
			if completed != tt.wantCompleted || stop != tt.wantStop {
				t.Errorf("completed/stop = %v/%v, want %v/%v", completed, stop, tt.wantCompleted, tt.wantStop)
			}
			if got := len(runner.usage) == 1; got != tt.wantUsage {
				t.Errorf("usage recorded = %v, want %v", got, tt.wantUsage)
			}
		})
	}
}

func TestMainTaskStopsWhenBudgetIsSpent(t *testing.T) {
	rec := claudeclitest.Install(t, claudeclitest.Script{})
	runner := &recordingRunner{spent: 1001}
	sess := session.New(t.TempDir(), "main", "sonnet", session.SessionOptions{ReviewMode: "diff"})
	task := NewMainTask(MainTaskDeps{Runner: runner, Session: sess, ToolDefs: testToolDefs, MaxTokensBudget: 1000})

	completed, stop, err := task.RunMainTask(context.Background(), mainMessages, "g")
	if err != nil || completed || stop != llmloop.StopTokenBudget {
		t.Fatalf("RunMainTask = %v/%v/%v, want false/StopTokenBudget/nil", completed, stop, err)
	}
	if n := len(rec.Invocations(t)); n != 0 {
		t.Errorf("claude invoked %d time(s) past the budget", n)
	}
}
