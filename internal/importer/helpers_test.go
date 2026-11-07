// Copyright 2026 The flux4swarm Authors
// SPDX-License-Identifier: Apache-2.0

package importer

import "github.com/olljanat-ai/flux4swarm/internal/api"

func specFor(path string) api.KustomizationSpec {
	return api.KustomizationSpec{Path: path}
}
