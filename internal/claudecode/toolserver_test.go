// SPDX-License-Identifier: Apache-2.0
// Copyright 2026 alibaba/open-code-review Contributors

package claudecode

import (
	"context"
	"encoding/json"
	"net/http"
	"os"
	"slices"
	"strings"
	"sync"
	"testing"

	"github.com/modelcontextprotocol/go-sdk/mcp"

	"github.com/alibaba/open-code-review/internal/llm"
	"github.com/alibaba/open-code-review/internal/session"
	"github.com/alibaba/open-code-review/internal/tool"
)

type recordingExecutor struct {
	mu    sync.Mutex
	calls []llm.ToolCall
	keys  []string
}

func (e *recordingExecutor) ExecuteTool(_ context.Context, taskKey string, call llm.ToolCall, _ *session.TaskRecord) tool.TaskCheckpoint {
	e.mu.Lock()
	defer e.mu.Unlock()
	e.calls = append(e.calls, call)
	e.keys = append(e.keys, taskKey)
	switch call.Function.Name {
	case tool.TaskDone.Name():
		return tool.Complete()
	case "explode":
		return tool.Fail("boom")
	case "silent":
		return tool.Of("")
	}
	return tool.Of("read " + call.Function.Arguments)
}

var testToolDefs = []llm.ToolDef{
	{Type: "function", Function: llm.FunctionDef{
		Name:        tool.FileRead.Name(),
		Description: "read a file",
		Parameters:  map[string]any{"type": "object", "properties": map[string]any{"path": map[string]any{"type": "string"}}},
	}},
	{Type: "function", Function: llm.FunctionDef{Name: tool.TaskDone.Name()}},
	{Type: "function", Function: llm.FunctionDef{Name: "explode"}},
	{Type: "function", Function: llm.FunctionDef{Name: "silent"}},
}

type bearer string

func (b bearer) RoundTrip(req *http.Request) (*http.Response, error) {
	req = req.Clone(req.Context())
	if b != "" {
		req.Header.Set("Authorization", "Bearer "+string(b))
	}
	return http.DefaultTransport.RoundTrip(req)
}

func connect(t *testing.T, srv *toolServer, token string) (*mcp.ClientSession, error) {
	t.Helper()
	transport := &mcp.StreamableClientTransport{Endpoint: srv.url, HTTPClient: &http.Client{Transport: bearer(token)}}
	return mcp.NewClient(&mcp.Implementation{Name: "test"}, nil).Connect(context.Background(), transport, nil)
}

func callText(t *testing.T, cs *mcp.ClientSession, name string, args map[string]any) (string, bool) {
	t.Helper()
	res, err := cs.CallTool(context.Background(), &mcp.CallToolParams{Name: name, Arguments: args})
	if err != nil {
		t.Fatalf("CallTool %s: %v", name, err)
	}
	var b strings.Builder
	for _, c := range res.Content {
		if tc, ok := c.(*mcp.TextContent); ok {
			b.WriteString(tc.Text)
		}
	}
	return b.String(), res.IsError
}

func TestToolServerExposesToolDefsAndForwardsCalls(t *testing.T) {
	exec := &recordingExecutor{}
	srv, err := startToolServer(testToolDefs, exec, "group-a", nil)
	if err != nil {
		t.Fatalf("startToolServer: %v", err)
	}
	defer srv.Close()

	cs, err := connect(t, srv, srv.token)
	if err != nil {
		t.Fatalf("connect: %v", err)
	}
	defer cs.Close()

	listed, err := cs.ListTools(context.Background(), nil)
	if err != nil {
		t.Fatalf("ListTools: %v", err)
	}
	var names []string
	for _, tl := range listed.Tools {
		names = append(names, tl.Name)
	}
	slices.Sort(names)
	if want := []string{"explode", "file_read", "silent", "task_done"}; !slices.Equal(names, want) {
		t.Errorf("tools = %v, want %v", names, want)
	}

	out, isErr := callText(t, cs, "file_read", map[string]any{"path": "a.go"})
	if isErr || out != `read {"path":"a.go"}` {
		t.Errorf("file_read = %q (error %v), want the executor's result", out, isErr)
	}
	if exec.keys[0] != "group-a" {
		t.Errorf("task key = %q, want group-a", exec.keys[0])
	}
	if srv.completed() {
		t.Error("completed before task_done")
	}

	if out, _ := callText(t, cs, "silent", nil); !strings.Contains(out, "no result") {
		t.Errorf("silent = %q, want a no-result notice", out)
	}
	if _, _ = callText(t, cs, "task_done", nil); !srv.completed() {
		t.Error("task_done did not mark the run completed")
	}
	if out, isErr := callText(t, cs, "explode", nil); !isErr || srv.failure() != "boom" {
		t.Errorf("explode = %q (error %v), failure %q; want an error result and failure boom", out, isErr, srv.failure())
	}
}

func TestToolServerRequiresToken(t *testing.T) {
	srv, err := startToolServer(testToolDefs, &recordingExecutor{}, "g", nil)
	if err != nil {
		t.Fatalf("startToolServer: %v", err)
	}
	defer srv.Close()

	for _, token := range []string{"", "wrong"} {
		if cs, err := connect(t, srv, token); err == nil {
			cs.Close()
			t.Errorf("connected with token %q; want a refusal", token)
		}
	}
	if !strings.HasPrefix(srv.url, "http://127.0.0.1:") {
		t.Errorf("url = %q, want a loopback address", srv.url)
	}
}

func TestToolServerWritesMCPConfig(t *testing.T) {
	srv, err := startToolServer(testToolDefs, &recordingExecutor{}, "g", nil)
	if err != nil {
		t.Fatalf("startToolServer: %v", err)
	}
	defer srv.Close()

	path, err := srv.writeConfig(t.TempDir())
	if err != nil {
		t.Fatalf("writeConfig: %v", err)
	}
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read config: %v", err)
	}
	var cfg struct {
		MCPServers map[string]struct {
			Type    string            `json:"type"`
			URL     string            `json:"url"`
			Headers map[string]string `json:"headers"`
		} `json:"mcpServers"`
	}
	if err := json.Unmarshal(data, &cfg); err != nil {
		t.Fatalf("decode config: %v", err)
	}
	entry, ok := cfg.MCPServers[serverName]
	if !ok || entry.Type != "http" || entry.URL != srv.url || entry.Headers["Authorization"] != "Bearer "+srv.token {
		t.Errorf("config = %s, want an http entry for %q with the bearer token", data, serverName)
	}
	if info, _ := os.Stat(path); info.Mode().Perm()&0o077 != 0 {
		t.Errorf("config mode = %v, want it private to the user", info.Mode().Perm())
	}
	if got := srv.allowedTools(); !slices.Contains(got, "mcp__ocr__file_read") || len(got) != len(testToolDefs) {
		t.Errorf("allowedTools = %v, want one mcp__ocr__ entry per tool", got)
	}
}

func TestToolServerAcceptsUntypedSchema(t *testing.T) {
	defs := []llm.ToolDef{{Type: "function", Function: llm.FunctionDef{
		Name:       "code_search",
		Parameters: map[string]any{"properties": map[string]any{"q": map[string]any{"type": "string"}}},
	}}}
	srv, err := startToolServer(defs, &recordingExecutor{}, "g", nil)
	if err != nil {
		t.Fatalf("startToolServer: %v", err)
	}
	defer srv.Close()

	cs, err := connect(t, srv, srv.token)
	if err != nil {
		t.Fatalf("connect: %v", err)
	}
	defer cs.Close()
	if out, isErr := callText(t, cs, "code_search", map[string]any{"q": "x"}); isErr || !strings.Contains(out, `"q":"x"`) {
		t.Errorf("code_search = %q (error %v), want the call forwarded", out, isErr)
	}
}
