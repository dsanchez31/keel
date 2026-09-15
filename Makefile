# KEEL build targets.

GO              ?= go
PNPM            ?= pnpm
BIN_DIR         ?= bin
CMDS            := keeld keelctl keelsim

# The reference mission. Its recording is not versioned: keelsim rebuilds it
# byte for byte from the scenario's seed, and cmd/keelsim/testdata pins its
# chain.
REFERENCE_SCENARIO := examples/sims/reference.yaml
DEMO_LOG           := examples/replays/run-042.log
DEMO_MISSION       := MSN-042
KEELD_DATA         ?= run/keeld

VERSION         ?= $(shell git describe --tags --always --dirty 2>/dev/null || echo dev)
COMMIT          ?= $(shell git rev-parse --short HEAD 2>/dev/null || echo unknown)
LDFLAGS         := -s -w \
                   -X github.com/dsanchez31/keel/internal/buildinfo.Version=$(VERSION) \
                   -X github.com/dsanchez31/keel/internal/buildinfo.Commit=$(COMMIT)

.PHONY: all build test lint check-sdk generate record-reference reference-log demo-replay clean tidy fmt web-install web-build web-lint web-test docker-up docker-down demo-video

all: build

## build: compile keeld, keelctl and keelsim into $(BIN_DIR)
build:
	@mkdir -p $(BIN_DIR)
	@for c in $(CMDS); do \
		echo "building $$c"; \
		$(GO) build -trimpath -ldflags '$(LDFLAGS)' -o $(BIN_DIR)/$$c ./cmd/$$c || exit 1; \
	done

## test: full Go test suite
test:
	$(GO) test ./...

## lint: golangci-lint, including the forbidigo determinism rules
lint:
	golangci-lint run ./...

## generate: regenerate the TypeScript wire types from the Go types
generate:
	$(GO) generate ./...

## check-sdk: regenerate the SDK types and fail on a non-empty diff
check-sdk: generate
	@if ! git diff --quiet --exit-code -- sdk-ts/src/generated; then \
		echo "sdk-ts/src/generated is out of date, run 'make generate' and commit the result"; \
		git --no-pager diff -- sdk-ts/src/generated; \
		exit 1; \
	fi
	@echo "sdk-ts/src/generated is up to date"

## record-reference: re-pin the reference mission's chain heads
## (after a deliberate change of behaviour: TestReferenceReplay fails until then)
record-reference:
	$(GO) test ./cmd/keelsim -run '^TestReferenceReplay$$' -count=1 -update-reference

## reference-log: record the reference mission into $(DEMO_LOG), for keelctl
## replay and doctrine diff, refusing an incomplete run
reference-log:
	@tmp=$$(mktemp -d) && \
	$(GO) run ./cmd/keelsim --scenario $(REFERENCE_SCENARIO) --quiet --out $$tmp/log && \
	mkdir -p $(dir $(DEMO_LOG)) && \
	cat $$tmp/log/*.keellog > $(DEMO_LOG); \
	status=$$?; rm -rf $$tmp; exit $$status

## demo-replay: hand the reference recording to a local keeld as a finished
## mission, for the frontend's replay route (/replays/MSN-042)
demo-replay: reference-log
	@dir=$(KEELD_DATA)/missions/$(DEMO_MISSION); \
	if [ -e $$dir ] && ! cmp -s $(DEMO_LOG) $$dir/00000000.keellog; then \
		echo "$$dir already holds another log, remove it first"; exit 1; \
	fi; \
	mkdir -p $$dir && cp $(DEMO_LOG) $$dir/00000000.keellog && echo "$(DEMO_MISSION) ready under $$dir"

## tidy: sync go.mod and go.sum
tidy:
	$(GO) mod tidy

## fmt: gofmt the tree
fmt:
	$(GO) fmt ./...

## web-install: install the pnpm workspace dependencies
web-install:
	$(PNPM) install

## web-build: build the frontend
web-build:
	$(PNPM) --filter @keel/web build

## web-lint: typecheck and lint the frontend and the SDK
web-lint:
	$(PNPM) -r typecheck
	$(PNPM) -r lint

## web-test: frontend and SDK test suites
web-test:
	$(PNPM) -r test

## docker-up: one-command startup of sitl, keelsim, keeld and web, the images
## stamped with the version and commit the Go binaries carry
docker-up:
	KEEL_VERSION=$(VERSION) KEEL_COMMIT=$(COMMIT) docker compose up --build

## docker-down: stop the stack and remove its volume, the recorded missions
docker-down:
	docker compose down -v

## demo-video: record the demo through the UI against the stack make docker-up
## runs (no mission running), into run/demo/keel-demo.mp4 (not versioned) and
## docs/media/demo.gif; the system Chrome and ffmpeg, nothing downloaded
demo-video:
	$(PNPM) --filter @keel/web demo-video

clean:
	rm -rf $(BIN_DIR)
