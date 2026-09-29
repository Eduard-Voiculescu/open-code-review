// SPDX-License-Identifier: Apache-2.0
// Copyright 2026 alibaba/open-code-review Contributors

package agent

import (
	"os"
	"testing"

	"github.com/alibaba/open-code-review/internal/claudecli/claudeclitest"
)

func TestMain(m *testing.M) {
	claudeclitest.Main()
	tmp, err := os.MkdirTemp("", "ocr-test-*")
	if err != nil {
		panic(err)
	}
	_ = os.Setenv("HOME", tmp)
	_ = os.Setenv("USERPROFILE", tmp)

	code := m.Run()

	_ = os.RemoveAll(tmp)
	os.Exit(code)
}
