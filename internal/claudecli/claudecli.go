// SPDX-License-Identifier: Apache-2.0
// Copyright 2026 alibaba/open-code-review Contributors

// Package claudecli runs the locally installed Claude Code CLI in headless
// print mode and decodes its final JSON result.
package claudecli

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"strconv"
	"strings"
	"time"
)

// BinaryEnv overrides the claude executable, which otherwise resolves from PATH.
const BinaryEnv = "OCR_CLAUDE_CODE_BIN"

// MCPOutputTokens lifts Claude Code's default 25k-token cap on one MCP tool
// result for tool runs. Output past the cap is spilled to a file meant for the
// built-in Read tool, which these sessions do not have.
const MCPOutputTokens = "100000"

const mcpOutputTokensEnv = "MAX_MCP_OUTPUT_TOKENS"

// ErrMaxTurns reports that the CLI stopped because it reached Request.MaxTurns.
var ErrMaxTurns = errors.New("claude reached its turn limit")

const waitDelay = 5 * time.Second

// Request describes one headless claude invocation. Empty fields are omitted
// from the command line.
type Request struct {
	SystemPrompt  string
	Prompt        string
	Model         string
	JSONSchema    json.RawMessage
	MCPConfigPath string
	AllowedTools  []string
	MaxTurns      int
}

// Usage is the token usage the CLI reports for the whole run.
type Usage struct {
	InputTokens              int64 `json:"input_tokens"`
	OutputTokens             int64 `json:"output_tokens"`
	CacheReadInputTokens     int64 `json:"cache_read_input_tokens"`
	CacheCreationInputTokens int64 `json:"cache_creation_input_tokens"`
}

// Result is the final message of a headless run.
type Result struct {
	Text             string          `json:"result"`
	StructuredOutput json.RawMessage `json:"structured_output"`
	Usage            Usage           `json:"usage"`
	NumTurns         int             `json:"num_turns"`
	Subtype          string          `json:"subtype"`
	IsError          bool            `json:"is_error"`
}

// Binary returns the claude executable to run.
func Binary() string {
	if bin := strings.TrimSpace(os.Getenv(BinaryEnv)); bin != "" {
		return bin
	}
	return "claude"
}

// Run executes claude with req and returns its decoded result. On ErrMaxTurns
// the partial result is returned too, since its usage still counts.
func Run(ctx context.Context, req Request) (*Result, error) {
	cmd := exec.CommandContext(ctx, Binary(), args(req)...)
	// A neutral working directory keeps the project's CLAUDE.md and
	// .claude settings out of the session; ocr's tools address the repo.
	cmd.Dir = os.TempDir()
	cmd.Stdin = strings.NewReader(req.Prompt)
	cmd.WaitDelay = waitDelay
	if req.MCPConfigPath != "" && strings.TrimSpace(os.Getenv(mcpOutputTokensEnv)) == "" {
		cmd.Env = append(os.Environ(), mcpOutputTokensEnv+"="+MCPOutputTokens)
	}
	var stdout, stderr bytes.Buffer
	cmd.Stdout = &stdout
	cmd.Stderr = &stderr

	runErr := cmd.Run()
	if ctx.Err() != nil {
		return nil, ctx.Err()
	}
	if errors.Is(runErr, exec.ErrNotFound) || errors.Is(runErr, os.ErrNotExist) {
		return nil, fmt.Errorf("claude CLI not found (%s): install Claude Code and log in with `claude` first: %w", Binary(), runErr)
	}

	var res Result
	if err := json.Unmarshal(stdout.Bytes(), &res); err != nil {
		if runErr != nil {
			return nil, fmt.Errorf("claude exited: %w: %s", runErr, strings.TrimSpace(stderr.String()))
		}
		return nil, fmt.Errorf("decode claude result: %w", err)
	}
	if res.Subtype == "error_max_turns" {
		return &res, ErrMaxTurns
	}
	if res.IsError {
		return nil, fmt.Errorf("claude returned an error (%s): %s", res.Subtype, strings.TrimSpace(res.Text))
	}
	if runErr != nil {
		return nil, fmt.Errorf("claude exited: %w: %s", runErr, strings.TrimSpace(stderr.String()))
	}
	return &res, nil
}

func args(req Request) []string {
	a := []string{
		"-p",
		"--output-format", "json",
		"--no-session-persistence",
		"--tools", "",
		"--setting-sources", "",
		"--strict-mcp-config",
	}
	if req.SystemPrompt != "" {
		a = append(a, "--system-prompt", req.SystemPrompt)
	}
	if req.Model != "" {
		a = append(a, "--model", req.Model)
	}
	if len(req.JSONSchema) > 0 {
		a = append(a, "--json-schema", string(req.JSONSchema))
	}
	if req.MCPConfigPath != "" {
		a = append(a, "--mcp-config", req.MCPConfigPath)
	}
	if len(req.AllowedTools) > 0 {
		a = append(a, "--allowedTools", strings.Join(req.AllowedTools, ","))
	}
	if req.MaxTurns > 0 {
		a = append(a, "--max-turns", strconv.Itoa(req.MaxTurns))
	}
	return a
}
