.PHONY: install gen dump-schema lint golangci run build dist agents dev deploy test ui-build clean verify-deps verify-docker

ADDR    ?= :6502
VERSION ?= $(shell git describe --always --dirty 2>/dev/null || echo dev)
SRCDIR  := $(abspath $(dir $(lastword $(MAKEFILE_LIST))))
LDFLAGS := -X main.version=$(VERSION) -X main.srcDir=$(SRCDIR)
# Agents run on OTHER machines, where this source path does not exist — stamping
# it would make a remote agent warn about a tree it cannot see.
AGENT_LDFLAGS := -X main.version=$(VERSION)

## --- codegen ---
gen:              ## All codegen: dump SDL + typed TS client + lint (no DB/server)
	./bin/gen

dump-schema:      ## Dump the gat GraphQL SDL to ui/gen/schema.graphql (no DB, no server)
	go run ./cmd/corrallm dump-graphql ui/gen/schema.graphql

lint:             ## Validate UI query snippets against the SDL snapshot
	cd ui && pnpm lint

golangci:         ## Go lint
	golangci-lint run ./...

## --- run ---
build:            ## Build the server binary
	go build -ldflags "$(LDFLAGS)" -o bin/corrallm ./cmd/corrallm

GOBIN   := $(shell go env GOBIN)
ifeq ($(GOBIN),)
GOBIN   := $(shell go env GOPATH)/bin
endif

install:          ## Install a source-stamped corrallm to $(GOBIN) (what the systemd unit runs)
	@mkdir -p $(GOBIN)
	go build -ldflags "$(LDFLAGS)" -o $(GOBIN)/corrallm ./cmd/corrallm
	@echo "installed $(GOBIN)/corrallm (warns when it predates $(SRCDIR))"
	@echo "restart the service to run it: corrallm service restart"

run: build        ## Build + run the server
	ADDR='$(ADDR)' ./bin/corrallm serve

dist:             ## Full deployable: build UI (→ ui/dist, served via --web-root) + the binary
	$(MAKE) ui-build
	go build -ldflags "$(LDFLAGS)" -o bin/corrallm ./cmd/corrallm

# Agent binaries the daemon serves to attached machines. CGO_ENABLED=0 because
# sqlite is modernc (pure Go), so these cross-compile from any host with no
# toolchain — which is what makes `curl <daemon>/install.sh | bash` work on a
# Mac without installing Go there.
# windows/amd64 is built but UNVERIFIED — see internal/host/platform_windows.go.
# It has never run on Windows because there is no Windows machine here. The
# process-tree handling is a Job Object port of the POSIX group logic, and the
# recipes that probe and build tools are bash, which Windows does not have.
AGENT_PLATFORMS ?= darwin/arm64 linux/amd64 linux/arm64 windows/amd64

agents:           ## Cross-compile agent binaries into bin/agents/ for the daemon to serve
	@mkdir -p bin/agents
	@for p in $(AGENT_PLATFORMS); do \
		os=$${p%/*}; arch=$${p#*/}; \
		echo "==> $$os/$$arch"; \
		ext=""; [ "$$os" = "windows" ] && ext=".exe"; \
		CGO_ENABLED=0 GOOS=$$os GOARCH=$$arch \
		  go build -ldflags "$(AGENT_LDFLAGS)" -o bin/agents/corrallm-$$os-$$arch$$ext ./cmd/corrallm || exit 1; \
	done
	@printf '%s' "$(VERSION)" > bin/agents/VERSION
	@ls -la bin/agents/

ui-build:         ## Typecheck + production-build the UI
	cd ui && pnpm install && pnpm build

deploy:           ## Build, restart the service, then publish the UI (delegates to bin/deploy)
	@./bin/deploy $(ARGS)

dev:              ## Frees :6502/:6503, runs air (Go hot-reload) + Vite (delegates to bin/dev)
	@ADDR='$(ADDR)' ./bin/dev

test:             ## Go tests
	go test ./...

# Proves corrallm builds from its OWN go.mod, with nothing borrowed from the
# machine it is sitting on.
#
# This exists because the alternative failed silently for a long time: go.mod
# declared gwag v1.1.0-rc.5 while a `replace` pointed at ../gwag, so every build
# here used whatever was checked out next door. The declared version had not
# been buildable in months — it lacks an API internal/api calls — and nothing
# noticed, because nothing ever tried. A dependency you never resolve is not
# pinned, it is imagined.
#
# GOWORK=off alone is not enough: a replace still redirects. Both are checked.
verify-deps:      ## Fail if the build depends on anything outside this module
	@echo "==> no replace directives"
	@bad=$$(GOWORK=off go list -m -f '{{if .Replace}}{{.Path}} => {{.Replace.Path}}{{end}}' all 2>/dev/null | grep . || true); \
	if [ -n "$$bad" ]; then \
		echo "   REPLACED (this build is not reproducible elsewhere):"; \
		echo "$$bad" | sed 's/^/     /'; \
		exit 1; \
	fi
	@echo "==> builds with the workspace disabled"
	@GOWORK=off go build ./... || exit 1
	@echo "==> go.mod is tidy"
	@cp go.mod $${TMPDIR:-/tmp}/corrallm.go.mod.bak; cp go.sum $${TMPDIR:-/tmp}/corrallm.go.sum.bak; \
	GOWORK=off go mod tidy; \
	rc=0; cmp -s go.mod $${TMPDIR:-/tmp}/corrallm.go.mod.bak || rc=1; \
	if [ $$rc -ne 0 ]; then \
		echo "   go.mod is not tidy — run 'GOWORK=off go mod tidy' and commit the result"; \
		cp $${TMPDIR:-/tmp}/corrallm.go.mod.bak go.mod; cp $${TMPDIR:-/tmp}/corrallm.go.sum.bak go.sum; \
		exit 1; \
	fi
	@echo "==> isolated: every dependency resolves from go.mod alone"
ifneq ($(VERIFY_DOCKER),0)
	@$(MAKE) --no-print-directory verify-docker
else
	@echo "==> docker check SKIPPED (VERIFY_DOCKER=0) — local checks only"
endif

# The Go version the container installs, taken from go.mod so the two cannot
# drift. `go 1.26.2` → `1.26.2`.
VERIFY_GO_VERSION := $(shell awk '/^go /{print $$2; exit}' go.mod)

# What gets verified. HEAD by default, because the claim being made is "a fresh
# clone builds". `git write-tree` on a staged index lets you check a change
# before committing it.
VERIFY_REF ?= HEAD

verify-docker:    ## Build, verify and test from a bare ubuntu:24.04 container
	@VERIFY_REF="$(VERIFY_REF)" ./scripts/verify/run.sh

clean:
	rm -f bin/corrallm
	rm -rf ui/dist
