.PHONY: gen dump-schema lint golangci run build dist agents dev test ui-build clean

ADDR    ?= :6502
VERSION ?= $(shell git describe --always --dirty 2>/dev/null || echo dev)
LDFLAGS := -X main.version=$(VERSION)

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

run: build        ## Build + run the server
	ADDR='$(ADDR)' ./bin/corrallm serve

dist:             ## Full deployable: build UI (→ ui/dist, served via --web-root) + the binary
	$(MAKE) ui-build
	go build -ldflags "$(LDFLAGS)" -o bin/corrallm ./cmd/corrallm

# Agent binaries the daemon serves to attached machines. CGO_ENABLED=0 because
# sqlite is modernc (pure Go), so these cross-compile from any host with no
# toolchain — which is what makes `curl <daemon>/install.sh | bash` work on a
# Mac without installing Go there.
AGENT_PLATFORMS ?= darwin/arm64 linux/amd64 linux/arm64

agents:           ## Cross-compile agent binaries into bin/agents/ for the daemon to serve
	@mkdir -p bin/agents
	@for p in $(AGENT_PLATFORMS); do \
		os=$${p%/*}; arch=$${p#*/}; \
		echo "==> $$os/$$arch"; \
		CGO_ENABLED=0 GOOS=$$os GOARCH=$$arch \
		  go build -ldflags "$(LDFLAGS)" -o bin/agents/corrallm-$$os-$$arch ./cmd/corrallm || exit 1; \
	done
	@printf '%s' "$(VERSION)" > bin/agents/VERSION
	@ls -la bin/agents/

ui-build:         ## Typecheck + production-build the UI
	cd ui && pnpm install && pnpm build

dev:              ## Frees :6502/:6503, runs air (Go hot-reload) + Vite (delegates to bin/dev)
	@ADDR='$(ADDR)' ./bin/dev

test:             ## Go tests
	go test ./...

clean:
	rm -f bin/corrallm
	rm -rf ui/dist
