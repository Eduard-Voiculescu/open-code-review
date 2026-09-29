// SPDX-License-Identifier: Apache-2.0
// Copyright 2026 alibaba/open-code-review Contributors

// Package claudecode drives ocr's main review task through the Claude Code
// CLI, serving ocr's own tools to it over an in-process MCP server.
package claudecode

import (
	"context"
	"crypto/rand"
	"crypto/subtle"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"maps"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"sync"
	"sync/atomic"
	"time"

	"github.com/modelcontextprotocol/go-sdk/mcp"

	"github.com/alibaba/open-code-review/internal/llm"
	"github.com/alibaba/open-code-review/internal/session"
	"github.com/alibaba/open-code-review/internal/tool"
)

const serverName = "ocr"

// ToolExecutor runs one tool call on behalf of a main task.
type ToolExecutor interface {
	ExecuteTool(ctx context.Context, taskKey string, call llm.ToolCall, rec *session.TaskRecord) tool.TaskCheckpoint
}

type toolServer struct {
	url   string
	token string
	names []string
	http  *http.Server

	done    atomic.Bool
	mu      sync.Mutex
	failMsg string
	callSeq atomic.Int64
}

func startToolServer(defs []llm.ToolDef, exec ToolExecutor, taskKey string, rec *session.TaskRecord) (*toolServer, error) {
	token, err := randomToken()
	if err != nil {
		return nil, err
	}
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		return nil, fmt.Errorf("listen for claude tool server: %w", err)
	}

	s := &toolServer{url: "http://" + ln.Addr().String() + "/mcp", token: token}
	mcpServer := mcp.NewServer(&mcp.Implementation{Name: serverName}, nil)
	for _, def := range defs {
		name := def.Function.Name
		mcpServer.AddTool(&mcp.Tool{Name: name, Description: def.Function.Description, InputSchema: objectSchema(def.Function.Parameters)},
			func(ctx context.Context, req *mcp.CallToolRequest) (*mcp.CallToolResult, error) {
				return s.handle(ctx, exec, taskKey, rec, name, req.Params.Arguments), nil
			})
		s.names = append(s.names, name)
	}

	handler := mcp.NewStreamableHTTPHandler(func(*http.Request) *mcp.Server { return mcpServer }, nil)
	s.http = &http.Server{Handler: s.authorize(handler), ReadHeaderTimeout: 10 * time.Second}
	go func() { _ = s.http.Serve(ln) }()
	return s, nil
}

func (s *toolServer) handle(ctx context.Context, exec ToolExecutor, taskKey string, rec *session.TaskRecord, name string, args json.RawMessage) *mcp.CallToolResult {
	if len(args) == 0 {
		args = json.RawMessage("{}")
	}
	cp := exec.ExecuteTool(ctx, taskKey, llm.ToolCall{
		ID:       fmt.Sprintf("claude-code-%d", s.callSeq.Add(1)),
		Type:     "function",
		Function: llm.FunctionCall{Name: name, Arguments: string(args)},
	}, rec)

	switch {
	case cp.Failed:
		s.mu.Lock()
		s.failMsg = cp.Data
		s.mu.Unlock()
		return textResult("Task failed: "+cp.Data, true)
	case cp.Completed:
		s.done.Store(true)
		return textResult("Task completed successfully. Stop now.", false)
	case cp.Data != "":
		return textResult(cp.Data, false)
	default:
		return textResult("Error: Tool execution returned no result.", false)
	}
}

// objectSchema returns params with "type":"object" set, which the MCP SDK
// panics without; tool configs may leave it implicit.
func objectSchema(params map[string]any) map[string]any {
	schema := make(map[string]any, len(params)+1)
	maps.Copy(schema, params)
	schema["type"] = "object"
	return schema
}

func textResult(text string, isError bool) *mcp.CallToolResult {
	return &mcp.CallToolResult{Content: []mcp.Content{&mcp.TextContent{Text: text}}, IsError: isError}
}

func (s *toolServer) authorize(next http.Handler) http.Handler {
	want := []byte("Bearer " + s.token)
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if subtle.ConstantTimeCompare([]byte(r.Header.Get("Authorization")), want) != 1 {
			http.Error(w, "unauthorized", http.StatusUnauthorized)
			return
		}
		next.ServeHTTP(w, r)
	})
}

func (s *toolServer) completed() bool { return s.done.Load() }

func (s *toolServer) failure() string {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.failMsg
}

// allowedTools names every served tool the way Claude Code addresses MCP tools.
func (s *toolServer) allowedTools() []string {
	out := make([]string, len(s.names))
	for i, name := range s.names {
		out[i] = "mcp__" + serverName + "__" + name
	}
	return out
}

// writeConfig writes the --mcp-config file into dir. It carries the bearer
// token, so it is readable by the current user only.
func (s *toolServer) writeConfig(dir string) (string, error) {
	cfg := map[string]any{"mcpServers": map[string]any{serverName: map[string]any{
		"type":    "http",
		"url":     s.url,
		"headers": map[string]string{"Authorization": "Bearer " + s.token},
	}}}
	data, err := json.Marshal(cfg)
	if err != nil {
		return "", err
	}
	path := filepath.Join(dir, "ocr-mcp.json")
	if err := os.WriteFile(path, data, 0o600); err != nil {
		return "", fmt.Errorf("write claude MCP config: %w", err)
	}
	return path, nil
}

func (s *toolServer) Close() error {
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	if err := s.http.Shutdown(ctx); err != nil && !errors.Is(err, http.ErrServerClosed) {
		return s.http.Close()
	}
	return nil
}

func randomToken() (string, error) {
	b := make([]byte, 32)
	if _, err := rand.Read(b); err != nil {
		return "", fmt.Errorf("generate claude tool server token: %w", err)
	}
	return hex.EncodeToString(b), nil
}
