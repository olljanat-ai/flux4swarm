# End-to-end test targets. Included from the root Makefile.
#   make e2e            build (if cmd/flux4swarm exists) -> up (Swarm + kind) -> test -> down
#   make e2e-up/e2e-down            Swarm (DinD) side
#   make e2e-k8s-up/e2e-k8s-down    kind + stock Flux2 + git server side
#   make e2e-test
# Variables: E2E_MODE=dind|host, E2E_K8S=0 (skip the K8s side), E2E_KEEP=1,
#            FLUX4SWARM_BIN=path, E2E_RUN=regex, E2E_DIND_PORT (default 23750),
#            E2E_GIT_PORT (default 23780), E2E_REGISTRY_PORT (default 23781),
#            E2E_GO_TEST_FLAGS.
# Tool and component versions are pinned in scripts/env.sh.

E2E_DIR        := $(patsubst %/,%,$(dir $(lastword $(MAKEFILE_LIST))))
E2E_ROOT       := $(abspath $(E2E_DIR)/../..)
E2E_MODE       ?= dind
E2E_K8S        ?= 1
E2E_DIND_PORT  ?= 23750
E2E_GIT_PORT   ?= 23780
E2E_REGISTRY_PORT ?= 23781
E2E_KEEP       ?=
E2E_RUN        ?=
E2E_GO_TEST_FLAGS ?= -v -count=1 -timeout 40m
FLUX4SWARM_BIN ?= $(E2E_ROOT)/bin/flux4swarm
E2E_BIN_DIR    ?= $(abspath $(E2E_DIR))/.bin

export E2E_MODE E2E_K8S E2E_DIND_PORT E2E_GIT_PORT E2E_REGISTRY_PORT FLUX4SWARM_BIN E2E_BIN_DIR

ifeq ($(E2E_MODE),dind)
E2E_DOCKER_HOST := tcp://127.0.0.1:$(E2E_DIND_PORT)
else
E2E_DOCKER_HOST := $(DOCKER_HOST)
endif

.PHONY: e2e e2e-build e2e-up e2e-down e2e-k8s-up e2e-k8s-down e2e-test

e2e-build:
	@if [ -d "$(E2E_ROOT)/cmd/flux4swarm" ]; then \
		$(MAKE) -C "$(E2E_ROOT)" build; \
	else \
		echo ">> cmd/flux4swarm not present; controller tests will be skipped"; \
	fi

e2e-up:
	"$(E2E_DIR)/scripts/up.sh"

e2e-down:
	"$(E2E_DIR)/scripts/down.sh"

e2e-k8s-up:
	"$(E2E_DIR)/scripts/k8s-up.sh"

e2e-k8s-down:
	"$(E2E_DIR)/scripts/k8s-down.sh"

# The pinned CLIs (kubectl, flux, ...) in E2E_BIN_DIR come first on PATH.
e2e-test:
	cd "$(E2E_DIR)" && DOCKER_HOST="$(E2E_DOCKER_HOST)" PATH="$(E2E_BIN_DIR):$$PATH" \
		go test -tags e2e $(E2E_GO_TEST_FLAGS) $(if $(E2E_RUN),-run '$(E2E_RUN)') ./...

# down always runs (unless E2E_KEEP=1), even if up or the tests fail; the
# first failing exit code is preserved.
e2e: e2e-build
	@rc=0; \
	{ $(MAKE) --no-print-directory e2e-up && \
	  { [ "$(E2E_K8S)" = "0" ] || $(MAKE) --no-print-directory e2e-k8s-up; } && \
	  $(MAKE) --no-print-directory e2e-test; } || rc=$$?; \
	if [ -n "$(E2E_KEEP)" ]; then \
		echo ">> E2E_KEEP set: leaving environment up (DOCKER_HOST=$(E2E_DOCKER_HOST), KUBECONFIG=$${E2E_STATE_DIR:-$${TMPDIR:-/tmp}/flux4swarm-e2e}/kubeconfig); run 'make e2e-down e2e-k8s-down' to clean up"; \
	else \
		$(MAKE) --no-print-directory e2e-down || true; \
		[ "$(E2E_K8S)" = "0" ] || $(MAKE) --no-print-directory e2e-k8s-down || true; \
	fi; \
	exit $$rc
