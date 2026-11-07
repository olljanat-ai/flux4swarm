// Copyright 2026 The flux4swarm Authors
// SPDX-License-Identifier: Apache-2.0

package notify

import (
	"crypto/hmac"
	"crypto/sha1"
	"crypto/sha256"
	"crypto/sha512"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"hash"
	"net/http"
	"strings"

	"github.com/olljanat-ai/flux4swarm/internal/api"
)

// Receiver types flux4swarm validates, with the same rules as Flux.
const (
	ReceiverGeneric     = "generic"
	ReceiverGenericHMAC = "generic-hmac"
	ReceiverGitHub      = "github"
	ReceiverGitLab      = "gitlab"
	ReceiverBitbucket   = "bitbucket"
	ReceiverHarbor      = "harbor"
	ReceiverDockerHub   = "dockerhub"
	ReceiverQuay        = "quay"
	ReceiverNexus       = "nexus"
	ReceiverACR         = "acr"
)

// SupportedReceiverTypes lists the Receiver types flux4swarm implements.
var SupportedReceiverTypes = []string{
	ReceiverGeneric, ReceiverGenericHMAC, ReceiverGitHub, ReceiverGitLab, ReceiverBitbucket,
	ReceiverHarbor, ReceiverDockerHub, ReceiverQuay, ReceiverNexus, ReceiverACR,
}

// WebhookPathPrefix is where Receivers are served.
const WebhookPathPrefix = "/hook/"

// MaxPayloadBytes limits the request body of a webhook.
const MaxPayloadBytes = 3 << 20

// WebhookPath is Flux's Receiver path: /hook/sha256(token+name+namespace).
func WebhookPath(token, name, namespace string) string {
	sum := sha256.Sum256([]byte(token + name + namespace))
	return WebhookPathPrefix + hex.EncodeToString(sum[:])
}

// ValidateReceiverSpec checks the parts of a Receiver that flux4swarm
// cannot honour.
func ValidateReceiverSpec(r *api.Receiver) error {
	supported := false
	for _, t := range SupportedReceiverTypes {
		if r.Spec.Type == t {
			supported = true
		}
	}
	if !supported {
		return fmt.Errorf("receiver type %q is not supported by flux4swarm (supported: %s)", r.Spec.Type, strings.Join(SupportedReceiverTypes, ", "))
	}
	if r.Spec.SecretRef == nil || r.Spec.SecretRef.Name == "" {
		return errors.New("spec.secretRef is required")
	}
	if r.Spec.ResourceFilter != "" {
		return errors.New("spec.resourceFilter (CEL) is not supported by flux4swarm")
	}
	for _, res := range r.Spec.Resources {
		if res.Filter != "" {
			return errors.New("spec.resources[].filter (CEL) is not supported by flux4swarm")
		}
		if res.Name == "*" && len(res.MatchLabels) == 0 {
			return fmt.Errorf("resource %s/*: matchLabels field not set when using wildcard '*' as name", res.Kind)
		}
	}
	return nil
}

// ValidateRequest authenticates a webhook request for the Receiver's type
// and checks its event type against spec.events. body is the request body.
func ValidateRequest(r *api.Receiver, token string, req *http.Request, body []byte) error {
	allowed := func(kind, event string) error {
		if len(r.Spec.Events) == 0 {
			return nil
		}
		for _, e := range r.Spec.Events {
			if strings.EqualFold(event, e) {
				return nil
			}
		}
		return fmt.Errorf("the %s event %q is not authorised", kind, event)
	}
	switch r.Spec.Type {
	case ReceiverGeneric:
		return nil
	case ReceiverGenericHMAC:
		if err := validateSignature(req.Header.Get("X-Signature"), body, []byte(token)); err != nil {
			return fmt.Errorf("unable to validate HMAC signature: %w", err)
		}
		return nil
	case ReceiverGitHub:
		sig := req.Header.Get("X-Hub-Signature-256")
		if sig == "" {
			sig = req.Header.Get("X-Hub-Signature")
		}
		if err := validateSignature(sig, body, []byte(token)); err != nil {
			return fmt.Errorf("the GitHub signature header is invalid, err: %w", err)
		}
		return allowed("GitHub", req.Header.Get("X-GitHub-Event"))
	case ReceiverGitLab:
		if !hmac.Equal([]byte(req.Header.Get("X-Gitlab-Token")), []byte(token)) {
			return errors.New("the X-Gitlab-Token header value does not match the receiver token")
		}
		return allowed("GitLab", req.Header.Get("X-Gitlab-Event"))
	case ReceiverBitbucket:
		if err := validateSignature(req.Header.Get("X-Hub-Signature"), body, []byte(token)); err != nil {
			return fmt.Errorf("the Bitbucket server signature header is invalid, err: %w", err)
		}
		return allowed("Bitbucket server", req.Header.Get("X-Event-Key"))
	case ReceiverHarbor:
		if !hmac.Equal([]byte(req.Header.Get("Authorization")), []byte(token)) {
			return errors.New("the Harbor Authorization header value does not match the receiver token")
		}
		return nil
	case ReceiverDockerHub:
		var p struct {
			PushData struct {
				Tag string `json:"tag"`
			} `json:"push_data"`
			Repository struct {
				URL string `json:"repo_url"`
			} `json:"repository"`
		}
		if err := json.Unmarshal(body, &p); err != nil {
			return errors.New("cannot decode DockerHub webhook payload")
		}
		return nil
	case ReceiverQuay:
		var p struct {
			DockerURL   string   `json:"docker_url"`
			UpdatedTags []string `json:"updated_tags"`
		}
		if err := json.Unmarshal(body, &p); err != nil {
			return fmt.Errorf("cannot decode Quay webhook payload: %w", err)
		}
		return nil
	case ReceiverNexus:
		sig := req.Header.Get("X-Nexus-Webhook-Signature")
		if sig == "" {
			return errors.New("the Nexus signature is missing from header")
		}
		m := hmac.New(sha1.New, []byte(token))
		m.Write(body)
		if !hmac.Equal([]byte(sig), []byte(hex.EncodeToString(m.Sum(nil)))) {
			return errors.New("invalid Nexus signature")
		}
		var p struct {
			Action         string `json:"action"`
			RepositoryName string `json:"repositoryName"`
		}
		if err := json.Unmarshal(body, &p); err != nil {
			return fmt.Errorf("cannot decode Nexus webhook payload: %w", err)
		}
		return nil
	case ReceiverACR:
		var p struct {
			Action string `json:"action"`
			Target struct {
				Repository string `json:"repository"`
				Tag        string `json:"tag"`
			} `json:"target"`
		}
		if err := json.Unmarshal(body, &p); err != nil {
			return fmt.Errorf("cannot decode ACR webhook payload: %w", err)
		}
		return nil
	}
	return fmt.Errorf("receiver type %q not supported", r.Spec.Type)
}

// validateSignature checks a "<algo>=<hex hmac>" signature of payload
// (sha1, sha256 or sha512), like go-github's ValidateSignature.
func validateSignature(sig string, payload, key []byte) error {
	algo, hexSig, ok := strings.Cut(sig, "=")
	if !ok || sig == "" {
		return errors.New("missing signature")
	}
	var h func() hash.Hash
	switch algo {
	case "sha1":
		h = sha1.New
	case "sha256":
		h = sha256.New
	case "sha512":
		h = sha512.New
	default:
		return fmt.Errorf("unknown hash type prefix: %q", algo)
	}
	got, err := hex.DecodeString(hexSig)
	if err != nil {
		return fmt.Errorf("error decoding signature %q: %w", hexSig, err)
	}
	m := hmac.New(h, key)
	m.Write(payload)
	if !hmac.Equal(got, m.Sum(nil)) {
		return errors.New("payload signature check failed")
	}
	return nil
}
