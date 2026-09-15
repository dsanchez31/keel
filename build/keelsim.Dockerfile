# syntax=docker/dockerfile:1

# keelsim, the simulator (spec.md section 15), on a distroless base with no
# shell and no package manager. In the stack it runs with --serve: the
# scenario's fleet over the native protocol and the fault endpoint, for keeld
# to dial (spec.md section 15.4).
#
# Build context: the repository root. docker-compose.yml passes VERSION and
# COMMIT; by hand:
#
#   docker build -f build/keelsim.Dockerfile \
#     --build-arg VERSION=$(git describe --tags --always --dirty) \
#     --build-arg COMMIT=$(git rev-parse --short HEAD) -t keel-keelsim .
#
# The image carries the binary only. The scenario, the world, the plan and the
# doctrine packs are mounted under /keel, the working directory, so keelsim's
# defaults (--scenario examples/sims/reference.yaml, --doctrine-dir
# doctrine-packs) resolve.
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
      -o /out/keelsim ./cmd/keelsim

FROM gcr.io/distroless/static-debian13:nonroot
COPY --from=build /out/keelsim /usr/local/bin/keelsim
WORKDIR /keel
# The vehicles (/v1/vectors/{id}) and the fault endpoint (/v1/faults).
EXPOSE 8090
ENTRYPOINT ["/usr/local/bin/keelsim"]
