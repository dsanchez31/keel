# syntax=docker/dockerfile:1

# keeld, the orchestrator daemon (spec.md section 16), on a distroless base
# with no shell and no package manager.
#
# Build context: the repository root. docker-compose.yml passes VERSION and
# COMMIT; by hand:
#
#   docker build -f build/keeld.Dockerfile \
#     --build-arg VERSION=$(git describe --tags --always --dirty) \
#     --build-arg COMMIT=$(git rev-parse --short HEAD) -t keel-keeld .
#
# The image carries the binary only. The configuration, the world, the
# doctrine packs and the mission logs are mounted under /keel, the working
# directory, so the relative paths of examples/keeld/*.yaml hold and keeld's
# default --config (examples/keeld/keelsim.yaml) resolves.
#
# The demo target (--target demo, what docker-compose.yml builds) adds the
# reference mission as the finished mission MSN-042, recorded at build time by
# keelsim from the scenario's seed rather than versioned: the same bytes as
# `make reference-log`, whose chain cmd/keelsim/testdata pins.
#
# Base images are pinned by tag, not by digest, by decision:
# a tag can be republished with different content between two builds, so two
# builds of one commit may differ. The durable fix is tag@sha256 on every FROM,
# as build/sitl.Dockerfile does, bumped together with the tag; it becomes
# necessary as soon as an image must be rebuilt bit for bit (a release, an
# audit).

ARG GO_VERSION=1.26

FROM golang:${GO_VERSION}-trixie AS build
WORKDIR /src
COPY go.mod go.sum ./
RUN --mount=type=cache,target=/go/pkg/mod go mod download
COPY cmd ./cmd
COPY internal ./internal
COPY vector ./vector
COPY adapters ./adapters
ARG VERSION=dev
ARG COMMIT=unknown
# The flags of the Makefile's build target: a static binary, no local paths,
# version and commit stamped into internal/buildinfo.
RUN --mount=type=cache,target=/go/pkg/mod \
    --mount=type=cache,target=/root/.cache/go-build \
    CGO_ENABLED=0 go build -trimpath \
      -ldflags "-s -w -X github.com/dsanchez31/keel/internal/buildinfo.Version=${VERSION} -X github.com/dsanchez31/keel/internal/buildinfo.Commit=${COMMIT}" \
      -o /out/keeld ./cmd/keeld \
 && mkdir -p /out/run/keeld/missions

# The reference mission, recorded by the keelsim of this same source into the
# segments keeld reads a mission from. An incomplete run fails the build.
FROM build AS reference
COPY examples/sims ./examples/sims
COPY examples/worlds ./examples/worlds
COPY examples/plans ./examples/plans
COPY doctrine-packs ./doctrine-packs
RUN --mount=type=cache,target=/go/pkg/mod \
    --mount=type=cache,target=/root/.cache/go-build \
    CGO_ENABLED=0 go build -trimpath -o /out/keelsim ./cmd/keelsim \
 && /out/keelsim --scenario examples/sims/reference.yaml --doctrine-dir doctrine-packs \
      --quiet --out /out/reference/MSN-042

FROM gcr.io/distroless/static-debian13:nonroot AS runtime
COPY --from=build /out/keeld /usr/local/bin/keeld
# The mission logs, owned by the image's user, laid out as examples/keeld/*.yaml
# expect them (data_dir run/keeld, logs under missions/). A named volume
# mounted on /keel/run starts with this content and ownership, so keeld records
# without running as root.
COPY --from=build --chown=nonroot:nonroot /out/run /keel/run
WORKDIR /keel
# The REST API and the stream.
EXPOSE 8080
ENTRYPOINT ["/usr/local/bin/keeld"]

# A volume created from this target starts with MSN-042 among the missions,
# so the first live mission is MSN-043. Docker copies an image's content into
# a named volume only while the volume is empty: one created by an earlier
# stack keeps what it held until `make docker-down` removes it.
FROM runtime AS demo
COPY --from=reference --chown=nonroot:nonroot /out/reference /keel/run/keeld/missions

# The default target: keeld without the demo mission.
FROM runtime
