// Copyright 2026 The flux4swarm Authors
// SPDX-License-Identifier: Apache-2.0

package main

import (
	"testing"
	"time"
)

func TestSyncSchedule(t *testing.T) {
	for d, want := range map[time.Duration]string{
		30 * time.Second: "*/1 * * * *",
		5 * time.Minute:  "*/5 * * * *",
		7 * time.Minute:  "*/10 * * * *",
		15 * time.Minute: "*/15 * * * *",
		45 * time.Minute: "0 * * * *",
		time.Hour:        "0 * * * *",
		5 * time.Hour:    "0 */6 * * *",
		48 * time.Hour:   "0 0 * * *",
	} {
		if got := syncSchedule(d); got != want {
			t.Errorf("syncSchedule(%s) = %q, want %q", d, got, want)
		}
	}
}
