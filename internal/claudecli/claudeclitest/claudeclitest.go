// SPDX-License-Identifier: Apache-2.0
// Copyright 2026 alibaba/open-code-review Contributors

// Package claudeclitest turns a Go test binary into a scripted fake of the
// claude CLI, so tests exercise the real process boundary on every platform.
//
// A package opts in by calling Main at the top of its TestMain; Install then
// points claudecli at the running test binary.
package claudeclitest

import (
	"bufio"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/alibaba/open-code-review/internal/claudecli"
	"github.com/modelcontextprotocol/go-sdk/mcp"
)

const (
	scriptEnv = "OCR_CLAUDECLITEST_SCRIPT"
	recordEnv = "OCR_CLAUDECLITEST_RECORD"
)

// Kind classifies an invocation by the flags it carries.
type Kind string

const (
	KindText   Kind = "text"
	KindSchema Kind = "schema"
	KindAgent  Kind = "agent"
)

// ToolCall is an MCP tool call the fake makes before printing its result.
type ToolCall struct {
	Name string         `json:"name"`
	Args map[string]any `json:"args"`
}

// Reply scripts one fake run.
type Reply struct {
	Stdout    string     `json:"stdout"`
	Stderr    string     `json:"stderr"`
	ExitCode  int        `json:"exit_code"`
	Hang      bool       `json:"hang"`
	ToolCalls []ToolCall `json:"tool_calls"`
}

// Script maps each invocation kind to its reply.
type Script map[Kind]Reply

// Invocation is what one fake run received.
type Invocation struct {
	Kind            Kind     `json:"kind"`
	Args            []string `json:"args"`
	Stdin           string   `json:"stdin"`
	Dir             string   `json:"dir"`
	ToolResults     []string `json:"tool_results"`
	ToolError       string   `json:"tool_error"`
	MCPOutputTokens string   `json:"mcp_output_tokens"`
}

// Recorder reads back the invocations of one Install.
type Recorder struct {
	path string
	mu   sync.Mutex
}

// Invocations returns every recorded run in order.
func (r *Recorder) Invocations(t *testing.T) []Invocation {
	t.Helper()
	r.mu.Lock()
	defer r.mu.Unlock()
	f, err := os.Open(r.path)
	if os.IsNotExist(err) {
		return nil
	}
	if err != nil {
		t.Fatalf("open record: %v", err)
	}
	defer f.Close()
	var out []Invocation
	sc := bufio.NewScanner(f)
	sc.Buffer(make([]byte, 0, 1<<20), 64<<20)
	for sc.Scan() {
		var inv Invocation
		if err := json.Unmarshal(sc.Bytes(), &inv); err != nil {
			t.Fatalf("decode record: %v", err)
		}
		out = append(out, inv)
	}
	return out
}

// Install routes claudecli to the fake for the rest of the test.
func Install(t *testing.T, script Script) *Recorder {
	t.Helper()
	exe, err := os.Executable()
	if err != nil {
		t.Fatalf("os.Executable: %v", err)
	}
	dir := t.TempDir()
	scriptPath := filepath.Join(dir, "script.json")
	data, err := json.Marshal(script)
	if err != nil {
		t.Fatalf("marshal script: %v", err)
	}
	if err := os.WriteFile(scriptPath, data, 0o600); err != nil {
		t.Fatalf("write script: %v", err)
	}
	rec := &Recorder{path: filepath.Join(dir, "record.jsonl")}
	t.Setenv(claudecli.BinaryEnv, exe)
	t.Setenv(scriptEnv, scriptPath)
	t.Setenv(recordEnv, rec.path)
	return rec
}

// Result renders a successful claude JSON result.
func Result(text string, structured any, inputTokens, outputTokens int64) string {
	res := map[string]any{
		"type":      "result",
		"subtype":   "success",
		"is_error":  false,
		"num_turns": 1,
		"result":    text,
		"usage":     map[string]any{"input_tokens": inputTokens, "output_tokens": outputTokens},
	}
	if structured != nil {
		res["structured_output"] = structured
	}
	data, _ := json.Marshal(res)
	return string(data)
}

// Main runs the fake and exits when the process was started by Install.
func Main() {
	path := os.Getenv(scriptEnv)
	if path == "" {
		return
	}
	os.Exit(runFake(path))
}

func runFake(scriptPath string) int {
	var script Script
	data, err := os.ReadFile(scriptPath)
	if err == nil {
		err = json.Unmarshal(data, &script)
	}
	if err != nil {
		fmt.Fprintf(os.Stderr, "claudeclitest: load script: %v\n", err)
		return 97
	}
	stdin, _ := io.ReadAll(os.Stdin)
	dir, _ := os.Getwd()
	args := os.Args[1:]
	inv := Invocation{Kind: classify(args), Args: args, Stdin: string(stdin), Dir: dir, MCPOutputTokens: os.Getenv("MAX_MCP_OUTPUT_TOKENS")}
	reply, ok := script[inv.Kind]
	if !ok {
		fmt.Fprintf(os.Stderr, "claudeclitest: no reply scripted for %s\n", inv.Kind)
		return 98
	}
	if len(reply.ToolCalls) > 0 {
		inv.ToolResults, err = callTools(args, reply.ToolCalls)
		if err != nil {
			inv.ToolError = err.Error()
		}
	}
	record(inv)
	if reply.Hang {
		time.Sleep(time.Minute)
	}
	fmt.Fprint(os.Stderr, reply.Stderr)
	fmt.Fprint(os.Stdout, reply.Stdout)
	return reply.ExitCode
}

func classify(args []string) Kind {
	switch {
	case slices.Contains(args, "--mcp-config"):
		return KindAgent
	case slices.Contains(args, "--json-schema"):
		return KindSchema
	default:
		return KindText
	}
}

func record(inv Invocation) {
	f, err := os.OpenFile(os.Getenv(recordEnv), os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0o600)
	if err != nil {
		return
	}
	defer f.Close()
	data, _ := json.Marshal(inv)
	_, _ = f.Write(append(data, '\n'))
}

type mcpConfig struct {
	MCPServers map[string]struct {
		URL     string            `json:"url"`
		Headers map[string]string `json:"headers"`
	} `json:"mcpServers"`
}

func callTools(args []string, calls []ToolCall) ([]string, error) {
	i := slices.Index(args, "--mcp-config")
	data, err := os.ReadFile(args[i+1])
	if err != nil {
		return nil, err
	}
	var cfg mcpConfig
	if err := json.Unmarshal(data, &cfg); err != nil {
		return nil, err
	}
	ctx := context.Background()
	var results []string
	for _, srv := range cfg.MCPServers {
		transport := &mcp.StreamableClientTransport{
			Endpoint:   srv.URL,
			HTTPClient: &http.Client{Transport: headerTransport(srv.Headers)},
		}
		session, err := mcp.NewClient(&mcp.Implementation{Name: "claudeclitest"}, nil).Connect(ctx, transport, nil)
		if err != nil {
			return nil, err
		}
		defer session.Close()
		for _, call := range calls {
			res, err := session.CallTool(ctx, &mcp.CallToolParams{Name: call.Name, Arguments: call.Args})
			if err != nil {
				return results, err
			}
			var text strings.Builder
			for _, c := range res.Content {
				if tc, ok := c.(*mcp.TextContent); ok {
					text.WriteString(tc.Text)
				}
			}
			results = append(results, text.String())
		}
	}
	return results, nil
}

type headerTransport map[string]string

func (h headerTransport) RoundTrip(req *http.Request) (*http.Response, error) {
	req = req.Clone(req.Context())
	for k, v := range h {
		req.Header.Set(k, v)
	}
	return http.DefaultTransport.RoundTrip(req)
}
