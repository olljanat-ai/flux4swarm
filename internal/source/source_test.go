// Copyright 2026 The flux4swarm Authors
// SPDX-License-Identifier: Apache-2.0

package source

import "testing"

func TestLatestVersion(t *testing.T) {
	tags := []string{"latest", "v1.0.0", "v1.2.0", "1.10.0-rc.1", "v1.9.3", "2.0.0", "main", "v1.9.3-dev"}
	cases := []struct {
		constraint, filter, want string
		wantErr                  bool
	}{
		{constraint: ">=1.0.0 <2.0.0", want: "v1.9.3"},
		{constraint: "^1.0", want: "v1.9.3"},
		{constraint: "~1.2", want: "v1.2.0"},
		{constraint: "*", want: "2.0.0"},
		{constraint: ">=1.10.0-0 <2.0.0", want: "1.10.0-rc.1"},
		{constraint: ">=1.0.0", filter: "^v", want: "v1.9.3"},
		{constraint: ">=3.0.0", wantErr: true},
		{constraint: "not a constraint", wantErr: true},
		{constraint: "*", filter: "(", wantErr: true},
	}
	for _, c := range cases {
		got, err := LatestVersion(tags, c.constraint, c.filter)
		if (err != nil) != c.wantErr || got != c.want {
			t.Errorf("LatestVersion(%q, %q) = %q, %v; want %q (error %v)", c.constraint, c.filter, got, err, c.want, c.wantErr)
		}
	}
}
