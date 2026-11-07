//go:build e2e

package e2e

import (
	"testing"
	"time"
)

// TestSwarmSmoke proves the harness itself works (no controller needed):
// the daemon at $DOCKER_HOST is an active swarm manager that can run services.
func TestSwarmSmoke(t *testing.T) {
	if got := docker(t, "info", "--format", "{{.Swarm.LocalNodeState}}"); got != "active" {
		t.Fatalf("swarm not active (LocalNodeState=%q); did you run scripts/up.sh and set DOCKER_HOST?", got)
	}
	if got := docker(t, "info", "--format", "{{.Swarm.ControlAvailable}}"); got != "true" {
		t.Fatalf("node is not a swarm manager (ControlAvailable=%q)", got)
	}
	if _, err := run(t, time.Minute, "", "git", "--version"); err != nil {
		t.Fatalf("git CLI not available: %v", err)
	}

	name := uniq("smoke")
	cleanupObjects(t, "service", name)
	docker(t, "service", "create", "--detach", "--name", name, "--label", e2eLabel,
		"--replicas", "1", imageV1)
	waitServiceRunning(t, name, 1)
	if s := mustService(t, name); s.image() != imageV1 || s.replicas() != 1 {
		t.Fatalf("unexpected service: image=%q replicas=%d", s.image(), s.replicas())
	}

	docker(t, "service", "rm", name)
	eventually(t, time.Minute, "service removed", func() (bool, string) {
		_, ok := inspectService(t, name)
		return !ok, "still exists"
	})
}
