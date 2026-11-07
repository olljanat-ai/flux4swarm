// Copyright 2026 The flux4swarm Authors
// SPDX-License-Identifier: Apache-2.0

package notify

import (
	"crypto/hmac"
	"crypto/sha1"
	"crypto/sha256"
	"encoding/hex"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/olljanat-ai/flux4swarm/internal/api"
)

func TestWebhookPath(t *testing.T) {
	sum := sha256.Sum256([]byte("tok" + "github" + "flux-system"))
	if got, want := WebhookPath("tok", "github", "flux-system"), "/hook/"+hex.EncodeToString(sum[:]); got != want {
		t.Errorf("got %s, want %s", got, want)
	}
}

func mac(algo string, key, body []byte) string {
	switch algo {
	case "sha1":
		m := hmac.New(sha1.New, key)
		m.Write(body)
		return hex.EncodeToString(m.Sum(nil))
	default:
		m := hmac.New(sha256.New, key)
		m.Write(body)
		return hex.EncodeToString(m.Sum(nil))
	}
}

func TestValidateRequest(t *testing.T) {
	body := []byte(`{"ref":"refs/heads/main"}`)
	token := "s3cret"
	rcv := func(typ string, events ...string) *api.Receiver {
		return &api.Receiver{Spec: api.ReceiverSpec{Type: typ, Events: events}}
	}
	cases := []struct {
		name    string
		r       *api.Receiver
		headers map[string]string
		body    []byte
		ok      bool
	}{
		{"generic", rcv(ReceiverGeneric), nil, body, true},
		{"hmac ok", rcv(ReceiverGenericHMAC), map[string]string{"X-Signature": "sha256=" + mac("sha256", []byte(token), body)}, body, true},
		{"hmac sha1", rcv(ReceiverGenericHMAC), map[string]string{"X-Signature": "sha1=" + mac("sha1", []byte(token), body)}, body, true},
		{"hmac bad", rcv(ReceiverGenericHMAC), map[string]string{"X-Signature": "sha256=" + mac("sha256", []byte("x"), body)}, body, false},
		{"hmac missing", rcv(ReceiverGenericHMAC), nil, body, false},
		{"github push", rcv(ReceiverGitHub, "ping", "push"), map[string]string{
			"X-Hub-Signature-256": "sha256=" + mac("sha256", []byte(token), body), "X-GitHub-Event": "push"}, body, true},
		{"github legacy sig", rcv(ReceiverGitHub), map[string]string{
			"X-Hub-Signature": "sha1=" + mac("sha1", []byte(token), body), "X-GitHub-Event": "push"}, body, true},
		{"github wrong event", rcv(ReceiverGitHub, "push"), map[string]string{
			"X-Hub-Signature-256": "sha256=" + mac("sha256", []byte(token), body), "X-GitHub-Event": "issues"}, body, false},
		{"github bad sig", rcv(ReceiverGitHub), map[string]string{"X-Hub-Signature-256": "sha256=00", "X-GitHub-Event": "push"}, body, false},
		{"gitlab", rcv(ReceiverGitLab, "Push Hook"), map[string]string{"X-Gitlab-Token": token, "X-Gitlab-Event": "push hook"}, body, true},
		{"gitlab bad token", rcv(ReceiverGitLab), map[string]string{"X-Gitlab-Token": "no"}, body, false},
		{"bitbucket", rcv(ReceiverBitbucket, "repo:refs_changed"), map[string]string{
			"X-Hub-Signature": "sha256=" + mac("sha256", []byte(token), body), "X-Event-Key": "repo:refs_changed"}, body, true},
		{"harbor", rcv(ReceiverHarbor), map[string]string{"Authorization": token}, body, true},
		{"harbor bad", rcv(ReceiverHarbor), map[string]string{"Authorization": "x"}, body, false},
		{"dockerhub", rcv(ReceiverDockerHub), nil, []byte(`{"push_data":{"tag":"1"}}`), true},
		{"dockerhub not json", rcv(ReceiverDockerHub), nil, []byte(`nope`), false},
		{"quay", rcv(ReceiverQuay), nil, []byte(`{"docker_url":"x"}`), true},
		{"acr", rcv(ReceiverACR), nil, []byte(`{"action":"push"}`), true},
		{"nexus", rcv(ReceiverNexus), map[string]string{"X-Nexus-Webhook-Signature": mac("sha1", []byte(token), body)}, body, true},
		{"nexus bad", rcv(ReceiverNexus), map[string]string{"X-Nexus-Webhook-Signature": "00"}, body, false},
		{"unsupported", rcv("gcr"), nil, body, false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			req := httptest.NewRequest("POST", "/hook/x", strings.NewReader(string(tc.body)))
			for k, v := range tc.headers {
				req.Header.Set(k, v)
			}
			err := ValidateRequest(tc.r, token, req, tc.body)
			if (err == nil) != tc.ok {
				t.Errorf("err = %v, want ok=%v", err, tc.ok)
			}
		})
	}
}

func TestValidateReceiverSpec(t *testing.T) {
	ok := &api.Receiver{Spec: api.ReceiverSpec{Type: "github", SecretRef: &api.LocalObjectReference{Name: "x"},
		Resources: []api.ReceiverResource{{NotificationObjectRef: api.NotificationObjectRef{Kind: "GitRepository", Name: "flux-system"}}}}}
	if err := ValidateReceiverSpec(ok); err != nil {
		t.Fatal(err)
	}
	bad := *ok
	bad.Spec.ResourceFilter = "true"
	if ValidateReceiverSpec(&bad) == nil {
		t.Error("resourceFilter should be rejected")
	}
	bad = *ok
	bad.Spec.Resources = []api.ReceiverResource{{NotificationObjectRef: api.NotificationObjectRef{Kind: "GitRepository", Name: "*"}}}
	if ValidateReceiverSpec(&bad) == nil {
		t.Error("wildcard without matchLabels should be rejected")
	}
	bad = *ok
	bad.Spec.SecretRef = nil
	if ValidateReceiverSpec(&bad) == nil {
		t.Error("missing secretRef should be rejected")
	}
}
