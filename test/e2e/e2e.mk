# End-to-end test targets. Included from the root Makefile.
#   make e2e            build (if cmd/flux4swarm exists) -> up -> test -> down
#   make e2e-up/e2e-down/e2e-test
# Variables: E2E_MODE=dind|host, E2E_KEEP=1, FLUX4SWARM_BIN=path, E2E_RUN=regex,
#            E2E_DIND_PORT (default 23750), E2E_GO_TEST_FLAGS.

E2E_DIR        := $(patsubst %/,%,$(dir $(lastword $(MAKEFILE_LIST))))
E2E_ROOT       := $(abspath $(E2E_DIR)/../..)
E2E_MODE       ?= dind
E2E_DIND_PORT  ?= 23750
E2E_KEEP       ?=
E2E_RUN        ?=
E2E_GO_TEST_FLAGS ?= -v -count=1 -timeout 20m
FLUX4SWARM_BIN ?= $(E2E_ROOT)/bin/flux4swarm

export E2E_MODE E2E_DIND_PORT FLUX4SWARM_BIN

ifeq ($(E2E_MODE),dind)
E2E_DOCKER_HOST := tcp://127.0.0.1:$(E2E_DIND_PORT)
else
E2E_DOCKER_HOST := $(DOCKER_HOST)
endif

.PHONY: e2e e2e-build e2e-up e2e-down e2e-test

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

e2e-test:
	cd "$(E2E_DIR)" && DOCKER_HOST="$(E2E_DOCKER_HOST)" \
		go test -tags e2e $(E2E_GO_TEST_FLAGS) $(if $(E2E_RUN),-run '$(E2E_RUN)') ./...

# down always runs (unless E2E_KEEP=1), even if up or the tests fail; the
# first failing exit code is preserved.
e2e: e2e-build
	@rc=0; \
	$(MAKE) --no-print-directory e2e-up && $(MAKE) --no-print-directory e2e-test || rc=$$?; \
	if [ -n "$(E2E_KEEP)" ]; then \
		echo ">> E2E_KEEP set: leaving environment up (DOCKER_HOST=$(E2E_DOCKER_HOST)); run 'make e2e-down' to clean up"; \
	else \
		$(MAKE) --no-print-directory e2e-down; \
	fi; \
	exit $$rc
