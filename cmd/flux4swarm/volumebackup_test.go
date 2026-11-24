// Copyright 2026 The flux4swarm Authors
// SPDX-License-Identifier: Apache-2.0

package main

import (
	"os"
	"path/filepath"
	"testing"
)

func TestResticEnv(t *testing.T) {
	f := filepath.Join(t.TempDir(), "password")
	if err := os.WriteFile(f, []byte("s3cret\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	t.Setenv("RESTIC_PASSWORD_FILE", f)
	if v, ok := resticEnv("RESTIC_PASSWORD"); !ok || v != "s3cret" {
		t.Errorf("from file: got %q, %v", v, ok)
	}
	t.Setenv("RESTIC_PASSWORD", "direct")
	if v, ok := resticEnv("RESTIC_PASSWORD"); !ok || v != "direct" {
		t.Errorf("env wins: got %q, %v", v, ok)
	}
	t.Setenv("FLUX4SWARM_TEST_KEY_FILE", filepath.Join(t.TempDir(), "missing"))
	if v, ok := resticEnv("FLUX4SWARM_TEST_KEY"); ok {
		t.Errorf("missing file: got %q, want unset", v)
	}
	if _, ok := resticEnv("FLUX4SWARM_TEST_UNSET"); ok {
		t.Error("unset: got set")
	}
}
