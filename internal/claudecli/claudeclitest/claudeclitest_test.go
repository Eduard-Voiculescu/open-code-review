// SPDX-License-Identifier: Apache-2.0
// Copyright 2026 alibaba/open-code-review Contributors

package claudeclitest

import (
	"encoding/json"
	"testing"
)

func TestClassify(t *testing.T) {
	tests := []struct {
		args []string
		want Kind
	}{
		{args: []string{"-p"}, want: KindText},
		{args: []string{"-p", "--json-schema", "{}"}, want: KindSchema},
		{args: []string{"-p", "--json-schema", "{}", "--mcp-config", "x.json"}, want: KindAgent},
	}
	for _, tt := range tests {
		if got := classify(tt.args); got != tt.want {
			t.Errorf("classify(%v) = %s, want %s", tt.args, got, tt.want)
		}
	}
}

func TestResult(t *testing.T) {
	var res struct {
		Subtype          string          `json:"subtype"`
		Result           string          `json:"result"`
		StructuredOutput json.RawMessage `json:"structured_output"`
		Usage            struct {
			InputTokens int64 `json:"input_tokens"`
		} `json:"usage"`
	}
	if err := json.Unmarshal([]byte(Result("hi", map[string]int{"n": 1}, 12, 3)), &res); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if res.Subtype != "success" || res.Result != "hi" || string(res.StructuredOutput) != `{"n":1}` || res.Usage.InputTokens != 12 {
		t.Errorf("Result = %+v, want a success carrying the given fields", res)
	}
}
