// SPDX-License-Identifier: Apache-2.0
// Copyright 2026 alibaba/open-code-review Contributors

package llmloop

import (
	"context"
	"strings"
	"testing"

	"github.com/alibaba/open-code-review/internal/llm"
	"github.com/alibaba/open-code-review/internal/session"
	"github.com/alibaba/open-code-review/internal/tool"
)

func TestExecuteToolMatchesMainLoopHandling(t *testing.T) {
	r, collector := newThinkingTestRunner(t, nil)
	rec := r.deps.Session.GetOrCreateFileSession("a.go").AppendTaskRecord(session.MainTask, nil)
	call := func(name, args string) tool.TaskCheckpoint {
		return r.ExecuteTool(context.Background(), "a.go", llm.ToolCall{
			ID:       "c1",
			Type:     "function",
			Function: llm.FunctionCall{Name: name, Arguments: args},
		}, rec)
	}

	cp := call(tool.CodeComment.Name(), `{"comments":[{"content":"nil deref","existing_code":"x.y"}]}`)
	if cp.Completed || cp.Failed || cp.Data == "" {
		t.Fatalf("code_comment checkpoint = %+v, want a plain result", cp)
	}
	if got := collector.CommentsForPath("a.go"); len(got) != 1 || got[0].Content != "nil deref" {
		t.Errorf("collector = %+v, want the comment filed under the task key", got)
	}
	if got := r.ToolCalls()[tool.CodeComment.Name()]; got != 1 {
		t.Errorf("ToolCalls[code_comment] = %d, want 1", got)
	}

	if cp := call(tool.TaskDone.Name(), `{}`); !cp.Completed {
		t.Errorf("task_done checkpoint = %+v, want Completed", cp)
	}
	if cp := call(tool.TaskDone.Name(), `{"state":"FAILED"}`); !cp.Failed {
		t.Errorf("task_done FAILED checkpoint = %+v, want Failed", cp)
	}
	if cp := call("no_such_tool", `{}`); !strings.Contains(cp.Data, tool.NotAvailableMsg) {
		t.Errorf("unknown tool checkpoint = %+v, want %q", cp, tool.NotAvailableMsg)
	}
}
