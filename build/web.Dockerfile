# syntax=docker/dockerfile:1

# The web UI: the Vite bundle of web/ (with @keel/sdk from the workspace),
# served by nginx, which also relays /api and the stream to keeld so the
# browser sees one origin, as the dev server's proxy does (web/vite.config.ts).
#
# Build context: the repository root:
#
#   docker build -f build/web.Dockerfile -t keel-web .
#
# At run time, KEELD_URL is keeld's address as nginx reaches it (default
# http://127.0.0.1:8080, keeld on the host network) and WEB_PORT the port the
# UI listens on (default 5173, the dev server's). The image's entrypoint
# substitutes both into build/web.nginx.conf.template.
#
# Base images are pinned by tag, not by digest, by decision:
# a tag can be republished with different content between two builds, so two
# builds of one commit may differ. The durable fix is tag@sha256 on every FROM,
# as build/sitl.Dockerfile does, bumped together with the tag; it becomes
# necessary as soon as an image must be rebuilt bit for bit (a release, an
# audit).

ARG NODE_VERSION=24
ARG NGINX_VERSION=1.30

FROM node:${NODE_VERSION}-trixie-slim AS build
# The pnpm the workspace pins (devEngines in package.json).
RUN npm install --global pnpm@12.4.1
WORKDIR /src
# Manifests first, so the dependency layer survives a source change.
COPY package.json pnpm-lock.yaml pnpm-workspace.yaml tsconfig.base.json ./
COPY web/package.json ./web/
COPY sdk-ts/package.json ./sdk-ts/
RUN --mount=type=cache,id=pnpm-store,target=/root/.local/share/pnpm/store \
    pnpm install --frozen-lockfile
COPY sdk-ts ./sdk-ts
COPY web ./web
# Typechecks, then bundles: web/dist, Cesium's assets under /cesium, the OSM
# extract under /basemap.
RUN pnpm --filter @keel/web build

FROM nginxinc/nginx-unprivileged:${NGINX_VERSION}-alpine
ENV KEELD_URL=http://127.0.0.1:8080 \
    WEB_PORT=5173
COPY build/web.nginx.conf.template /etc/nginx/templates/default.conf.template
COPY --from=build /src/web/dist /usr/share/nginx/html
EXPOSE 5173
