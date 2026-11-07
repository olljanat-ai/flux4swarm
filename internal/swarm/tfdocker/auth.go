// This Source Code Form is subject to the terms of the Mozilla Public
// License, v. 2.0. If a copy of the MPL was not distributed with this
// file, You can obtain one at https://mozilla.org/MPL/2.0/.
//
// SPDX-License-Identifier: MPL-2.0
//
// Derived from kreuzwerker/terraform-provider-docker,
// internal/provider/resource_docker_service_funcs.go (fromRegistryAuth,
// retrieveAndMarshalAuth), internal/provider/provider.go
// (isDockerHubRegistryHostname, dockerHubRegistryHostnames,
// getDockerHubAuthConfigFromMap) and internal/provider/helpers.go
// (containsIgnorableErrorMessage).
// Changes: exported the helpers, dropped the Terraform schema plumbing and
// added EncodedAuthForImage.

package tfdocker

import (
	"encoding/base64"
	"encoding/json"
	"strings"

	"github.com/docker/docker/api/types/registry"
)

// FromRegistryAuth extract the desired AuthConfiguration for the given image
func FromRegistryAuth(image string, authConfigs map[string]registry.AuthConfig) registry.AuthConfig {
	// Remove normalized prefixes to simplify substring
	image = strings.Replace(strings.Replace(image, "http://", "", 1), "https://", "", 1)
	// Get the registry with optional port
	lastBin := strings.Index(image, "/")
	// No auth given and image name has no slash like 'alpine:3.1'
	if lastBin != -1 {
		serverAddress := image[0:lastBin]
		if fromRegistryAuth, ok := authConfigs[serverAddress]; ok {
			return fromRegistryAuth
		}

		if IsDockerHubRegistryHostname(serverAddress) {
			if fromRegistryAuth, ok := getDockerHubAuthConfigFromMap(authConfigs); ok {
				return fromRegistryAuth
			}
		}
	} else if fromRegistryAuth, ok := getDockerHubAuthConfigFromMap(authConfigs); ok {
		// Images without a registry part come from Docker Hub.
		return fromRegistryAuth
	}

	return registry.AuthConfig{}
}

// EncodedAuthForImage returns the X-Registry-Auth header value for image,
// as passed in ServiceCreateOptions / ServiceUpdateOptions.
func EncodedAuthForImage(image string, authConfigs map[string]registry.AuthConfig) string {
	auth := FromRegistryAuth(image, authConfigs)
	marshalledAuth, _ := json.Marshal(auth) // https://docs.docker.com/engine/api/v1.37/#section/Versioning
	return base64.URLEncoding.EncodeToString(marshalledAuth)
}

// IsDockerHubRegistryHostname reports whether the host is Docker Hub.
func IsDockerHubRegistryHostname(registryHostname string) bool {
	for _, dockerHubHostname := range dockerHubRegistryHostnames() {
		if registryHostname == dockerHubHostname {
			return true
		}
	}

	return false
}

func dockerHubRegistryHostnames() []string {
	return []string{
		"registry-1.docker.io",
		"index.docker.io",
		"docker.io",
		"registry.hub.docker.com",
	}
}

func getDockerHubAuthConfigFromMap(authConfigs map[string]registry.AuthConfig) (registry.AuthConfig, bool) {
	for _, dockerHubHostname := range append(dockerHubRegistryHostnames(), "https://index.docker.io/v1/") {
		if authConfig, ok := authConfigs[dockerHubHostname]; ok {
			return authConfig, true
		}
	}

	return registry.AuthConfig{}, false
}

// ContainsIgnorableErrorMessage reports whether errorMsg contains any of the
// given messages, case-insensitively.
func ContainsIgnorableErrorMessage(errorMsg string, ignorableErrorMessages ...string) bool {
	for _, ignorableErrorMessage := range ignorableErrorMessages {
		if strings.Contains(strings.ToLower(errorMsg), strings.ToLower(ignorableErrorMessage)) {
			return true
		}
	}

	return false
}
