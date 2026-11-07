// Copyright 2026 The flux4swarm Authors
// SPDX-License-Identifier: Apache-2.0

// Package source holds what the Git and OCI fetchers share: the artifact
// type and semver tag selection.
package source

import (
	"fmt"
	"regexp"
	"sort"
	"time"

	"github.com/Masterminds/semver/v3"
)

// Artifact is a fetched revision of a source, unpacked in Dir.
type Artifact struct {
	// Dir is the root of the unpacked content.
	Dir string
	// Revision uses the Flux format, e.g. "main@sha1:<hash>" for Git or
	// "1.2.3@sha256:<digest>" for OCI.
	Revision string
	// Commit is the Git commit or the OCI manifest digest.
	Commit    string
	FetchedAt time.Time
}

// LatestVersion returns the tag with the highest semantic version that
// satisfies constraint (Masterminds/semver syntax, like Flux: ">=1.0.0 <2.0.0",
// "^1.2", "~1.2.3", "1.x", "*"). Tags that are not semantic versions are
// skipped. When filter is not empty, only tags matching that regular
// expression are considered (OCIRepository spec.ref.semverFilter).
func LatestVersion(tags []string, constraint, filter string) (string, error) {
	c, err := semver.NewConstraint(constraint)
	if err != nil {
		return "", fmt.Errorf("semver constraint %q: %w", constraint, err)
	}
	var re *regexp.Regexp
	if filter != "" {
		if re, err = regexp.Compile(filter); err != nil {
			return "", fmt.Errorf("semverFilter %q: %w", filter, err)
		}
	}
	type cand struct {
		v   *semver.Version
		tag string
	}
	var cands []cand
	for _, t := range tags {
		if re != nil && !re.MatchString(t) {
			continue
		}
		v, err := semver.NewVersion(t)
		if err != nil || !c.Check(v) {
			continue
		}
		cands = append(cands, cand{v, t})
	}
	if len(cands) == 0 {
		return "", fmt.Errorf("no tag matches semver constraint %q", constraint)
	}
	// Highest version wins; equal versions ("v1.0.0" and "1.0.0") are
	// decided by name so the choice is stable.
	sort.Slice(cands, func(i, j int) bool {
		if cmp := cands[i].v.Compare(cands[j].v); cmp != 0 {
			return cmp > 0
		}
		return cands[i].tag < cands[j].tag
	})
	return cands[0].tag, nil
}
