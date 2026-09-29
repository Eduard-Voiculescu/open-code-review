// SPDX-License-Identifier: Apache-2.0
// Copyright 2026 alibaba/open-code-review Contributors

package claudecode

import (
	"context"
	"errors"
	"fmt"
	"os"
	"time"

	"github.com/alibaba/open-code-review/internal/claudecli"
	"github.com/alibaba/open-code-review/internal/llm"
	"github.com/alibaba/open-code-review/internal/llmloop"
	"github.com/alibaba/open-code-review/internal/session"
)

const toolNamingHint = "Your tools are served by the \"ocr\" MCP server, so a tool named file_read is called as mcp__ocr__file_read. " +
	"You have no other tools; read the repository only through them."

// Runner executes tool calls and aggregates usage; *llmloop.Runner satisfies it.
type Runner interface {
	ToolExecutor
	RecordUsage(u *llm.UsageInfo)
	TotalTokensUsed() int64
}

// MainTaskDeps wires a MainTask to the review run it serves.
type MainTaskDeps struct {
	Runner   Runner
	Session  *session.SessionHistory
	ToolDefs []llm.ToolDef
	Model    string
	MaxTurns int
	// MaxTokensBudget is the run-wide token budget; 0 means unlimited.
	MaxTokensBudget int64
}

// MainTask runs one main-task conversation as a single headless claude
// session whose only tools are ocr's, served over MCP.
type MainTask struct {
	deps MainTaskDeps
}

// NewMainTask creates a MainTask.
func NewMainTask(deps MainTaskDeps) *MainTask {
	return &MainTask{deps: deps}
}

// RunMainTask mirrors llmloop.Runner.RunMainTask: completed reports task_done
// or a clean finish, and a turn limit maps to StopMaxRounds.
func (m *MainTask) RunMainTask(ctx context.Context, messages []llm.Message, taskKey string) (bool, llmloop.MainLoopStop, error) {
	if m.deps.MaxTokensBudget > 0 && m.deps.Runner.TotalTokensUsed() > m.deps.MaxTokensBudget {
		return false, llmloop.StopTokenBudget, nil
	}
	rec := m.deps.Session.GetOrCreateFileSession(taskKey).AppendTaskRecord(session.MainTask, append([]llm.Message(nil), messages...))
	start := time.Now()

	srv, err := startToolServer(m.deps.ToolDefs, m.deps.Runner, taskKey, rec)
	if err != nil {
		rec.SetError(err, time.Since(start))
		return false, llmloop.StopNone, err
	}
	defer srv.Close()

	dir, err := os.MkdirTemp("", "ocr-claude-")
	if err != nil {
		return false, llmloop.StopNone, fmt.Errorf("create claude config dir: %w", err)
	}
	defer os.RemoveAll(dir)
	cfgPath, err := srv.writeConfig(dir)
	if err != nil {
		return false, llmloop.StopNone, err
	}

	system, prompt := llm.ClaudeCodePrompt(messages)
	res, runErr := claudecli.Run(ctx, claudecli.Request{
		SystemPrompt:  system + "\n\n" + toolNamingHint,
		Prompt:        prompt,
		Model:         m.deps.Model,
		MCPConfigPath: cfgPath,
		AllowedTools:  srv.allowedTools(),
		MaxTurns:      m.deps.MaxTurns,
	})
	duration := time.Since(start)
	if res != nil {
		usage := llm.ClaudeCodeUsage(res.Usage)
		m.deps.Runner.RecordUsage(usage)
		text := res.Text
		rec.SetResponse(&llm.ChatResponse{
			Model:   m.deps.Model,
			Choices: []llm.Choice{{Message: llm.ResponseMessage{Role: "assistant", Content: &text}}},
			Usage:   usage,
		}, duration)
	} else if runErr != nil {
		rec.SetError(runErr, duration)
	}

	if msg := srv.failure(); msg != "" {
		return false, llmloop.StopNone, fmt.Errorf("task failed: %s", msg)
	}
	if srv.completed() {
		return true, llmloop.StopNone, nil
	}
	if errors.Is(runErr, claudecli.ErrMaxTurns) {
		return false, llmloop.StopMaxRounds, nil
	}
	if runErr != nil {
		return false, llmloop.StopNone, fmt.Errorf("claude code main task: %w", runErr)
	}
	// A clean exit with no tool call means claude never reached the MCP server
	// (proxy, connect timeout), so the diff went unread.
	if srv.callSeq.Load() == 0 {
		return false, llmloop.StopNone, fmt.Errorf("claude finished without calling any ocr tool: %s", res.Text)
	}
	return true, llmloop.StopNone, nil
}
