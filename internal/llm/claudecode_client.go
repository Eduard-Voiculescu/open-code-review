// SPDX-License-Identifier: Apache-2.0
// Copyright 2026 alibaba/open-code-review Contributors

package llm

import (
	"context"
	"encoding/json"
	"fmt"
	"slices"
	"strings"
	"time"

	"github.com/alibaba/open-code-review/internal/claudecli"
)

// ClaudeCodeClient serves single-shot requests through the claude CLI. A
// request carrying tools is answered with exactly one call, chosen and filled
// in as structured output; the multi-turn tool loop runs in internal/claudecode.
type ClaudeCodeClient struct {
	model   string
	timeout time.Duration
}

// schemaMaxTurns leaves room for the CLI's internal structured-output turn.
const schemaMaxTurns = 3

// NewClaudeCodeClient creates a client that shells out to the claude CLI.
func NewClaudeCodeClient(cfg ClientConfig) *ClaudeCodeClient {
	return &ClaudeCodeClient{model: cfg.Model, timeout: cfg.Timeout}
}

// CompletionsWithCtx runs one headless claude invocation for req.
func (c *ClaudeCodeClient) CompletionsWithCtx(ctx context.Context, req ChatRequest) (*ChatResponse, error) {
	if c.timeout > 0 {
		var cancel context.CancelFunc
		ctx, cancel = context.WithTimeout(ctx, c.timeout)
		defer cancel()
	}

	system, prompt := ClaudeCodePrompt(req.Messages)
	cliReq := claudecli.Request{
		SystemPrompt: system,
		Prompt:       prompt,
		Model:        c.model,
		MaxTurns:     1,
	}
	if req.Model != "" {
		cliReq.Model = req.Model
	}
	if len(req.Tools) > 0 {
		schema, err := toolChoiceSchema(req.Tools)
		if err != nil {
			return nil, err
		}
		cliReq.JSONSchema = schema
		cliReq.MaxTurns = schemaMaxTurns
	}

	res, err := claudecli.Run(ctx, cliReq)
	if err != nil {
		return nil, err
	}

	msg := ResponseMessage{Role: "assistant"}
	if len(req.Tools) > 0 {
		call, err := parseToolChoice(res.StructuredOutput, req.Tools)
		if err != nil {
			return nil, err
		}
		msg.ToolCalls = []ToolCall{call}
	} else {
		text := res.Text
		msg.Content = &text
	}
	return &ChatResponse{
		Model:   cliReq.Model,
		Choices: []Choice{{Message: msg, FinishReason: "stop"}},
		Usage:   ClaudeCodeUsage(res.Usage),
	}, nil
}

// toolChoiceSchema describes "call exactly one of these tools" as a JSON
// schema, keeping each tool's description where the model reads it.
func toolChoiceSchema(tools []ToolDef) (json.RawMessage, error) {
	branches := make([]any, 0, len(tools))
	for _, t := range tools {
		params := any(t.Function.Parameters)
		if t.Function.Parameters == nil {
			params = map[string]any{"type": "object"}
		}
		branches = append(branches, map[string]any{
			"type":        "object",
			"description": t.Function.Description,
			"properties": map[string]any{
				"tool":      map[string]any{"type": "string", "enum": []string{t.Function.Name}},
				"arguments": params,
			},
			"required": []string{"tool", "arguments"},
		})
	}
	schema, err := json.Marshal(map[string]any{
		"type":       "object",
		"properties": map[string]any{"call": map[string]any{"anyOf": branches}},
		"required":   []string{"call"},
	})
	if err != nil {
		return nil, fmt.Errorf("encode tool schema: %w", err)
	}
	return schema, nil
}

func parseToolChoice(raw json.RawMessage, tools []ToolDef) (ToolCall, error) {
	var out struct {
		Call struct {
			Tool      string          `json:"tool"`
			Arguments json.RawMessage `json:"arguments"`
		} `json:"call"`
	}
	if err := json.Unmarshal(raw, &out); err != nil {
		return ToolCall{}, fmt.Errorf("decode claude tool choice: %w", err)
	}
	if !slices.ContainsFunc(tools, func(t ToolDef) bool { return t.Function.Name == out.Call.Tool }) {
		return ToolCall{}, fmt.Errorf("claude chose tool %q, which the request did not offer", out.Call.Tool)
	}
	args := string(out.Call.Arguments)
	if args == "" || args == "null" {
		args = "{}"
	}
	return ToolCall{
		ID:       "claude-code-structured-output",
		Type:     "function",
		Function: FunctionCall{Name: out.Call.Tool, Arguments: args},
	}, nil
}

// ClaudeCodeUsage maps the CLI's usage onto UsageInfo the same way the
// Anthropic client counts cached input.
func ClaudeCodeUsage(u claudecli.Usage) *UsageInfo {
	usage := &UsageInfo{
		PromptTokens:     u.InputTokens + u.CacheReadInputTokens + u.CacheCreationInputTokens,
		CompletionTokens: u.OutputTokens,
		CacheReadTokens:  u.CacheReadInputTokens,
		CacheWriteTokens: u.CacheCreationInputTokens,
	}
	usage.TotalTokens = usage.PromptTokens + usage.CompletionTokens
	return usage
}

// ClaudeCodePrompt joins system messages into the system prompt and
// renders the rest as the prompt. A lone user message is passed verbatim;
// a conversation becomes a role-labelled transcript, since the CLI accepts a
// single prompt rather than a message list.
func ClaudeCodePrompt(messages []Message) (system, prompt string) {
	var systemParts []string
	var rest []Message
	for i := range messages {
		if messages[i].Role == "system" {
			systemParts = append(systemParts, messages[i].ExtractText())
			continue
		}
		rest = append(rest, messages[i])
	}
	system = strings.Join(systemParts, "\n\n")
	if len(rest) == 1 && rest[0].Role == "user" {
		return system, rest[0].ExtractText()
	}

	var b strings.Builder
	for i := range rest {
		m := &rest[i]
		if i > 0 {
			b.WriteString("\n\n")
		}
		fmt.Fprintf(&b, "[%s]\n", m.Role)
		b.WriteString(m.ExtractText())
		for _, tc := range m.ToolCalls {
			fmt.Fprintf(&b, "\n%s(%s)", tc.Function.Name, tc.Function.Arguments)
		}
	}
	return system, b.String()
}
