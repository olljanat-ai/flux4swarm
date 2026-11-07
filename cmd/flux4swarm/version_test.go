// Copyright 2026 The flux4swarm Authors
// SPDX-License-Identifier: Apache-2.0

package main

import (
	"bytes"
	"strings"
	"testing"
)

func TestVersionCmd(t *testing.T) {
	oldV, oldC, oldD := version, commit, date
	t.Cleanup(func() { version, commit, date = oldV, oldC, oldD })
	version, commit, date = "1.2.3", "abc123", "2026-01-02T03:04:05Z"

	run := func(args ...string) string {
		t.Helper()
		root := newRoot()
		var out bytes.Buffer
		root.SetOut(&out)
		root.SetArgs(args)
		if err := root.Execute(); err != nil {
			t.Fatal(err)
		}
		return out.String()
	}

	if got := run("version", "--short"); got != "1.2.3\n" {
		t.Errorf("version --short = %q", got)
	}
	got := run("version")
	for _, want := range []string{"version: 1.2.3\n", "commit:  abc123\n", "date:    2026-01-02T03:04:05Z\n", "go:      go"} {
		if !strings.Contains(got, want) {
			t.Errorf("version output %q lacks %q", got, want)
		}
	}
}
