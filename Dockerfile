# Previa release images (docs/25-production-deployment.md). One build stage
# compiles every command from source with generated templates and CSS; the
# runtime images hold only static binaries and run as UID 65532.
#
#   docker build --target server -t previa-server .
#   docker build --target worker -t previa-worker .
#   docker build --target egress -t previa-egress .
#
# Base images are pinned by digest; Tailwind is pinned by checksum
# (deploy/install-tailwind.sh); Go modules by go.sum.
ARG GO_IMAGE=golang:1.26.9-bookworm@sha256:d9c68c2c51161e12fd77e4c6320687c9cd86e1af1e3ad6e6cd63ff970641453c
ARG RUNTIME_IMAGE=gcr.io/distroless/static-debian12:nonroot@sha256:afa5c872c891853ca7fcf1f12c3edb23f7eeef36189728842dd51042ff57f7ab

# The build stage runs on the build machine's platform and cross-compiles,
# so multi-architecture releases need no emulation.
FROM --platform=$BUILDPLATFORM ${GO_IMAGE} AS build
ARG TARGETOS=linux TARGETARCH
WORKDIR /src
ENV CGO_ENABLED=0 GOTOOLCHAIN=local GOFLAGS=-trimpath
COPY go.mod go.sum ./
COPY tools/go.mod tools/go.sum tools/
RUN --mount=type=cache,target=/go/pkg/mod go mod download
COPY . .
ARG VERSION=dev
RUN --mount=type=cache,target=/go/pkg/mod --mount=type=cache,target=/root/.cache/go-build <<BUILD
set -eu
sh deploy/install-tailwind.sh bin/tailwindcss
bin/tailwindcss -i web/styles/app.css -o web/static/css/app.css --minify
go tool -modfile=tools/go.mod templ generate -path web
mkdir -p /out/bin /out/state/sources /out/state/router
for command in server worker migrate route-sync housekeeping secrets-reencrypt egress; do
  GOOS="${TARGETOS}" GOARCH="${TARGETARCH:-$(go env GOARCH)}" go build -buildvcs=false -ldflags="-s -w -X github.com/ajaykathwatee/previa/internal/platform/version.Version=${VERSION}" \
    -o "/out/bin/previa-${command}" "./cmd/${command}"
done
BUILD

FROM ${RUNTIME_IMAGE} AS server
COPY --from=build /out/bin/previa-server /out/bin/previa-migrate /out/bin/previa-secrets-reencrypt /usr/local/bin/
USER 65532:65532
EXPOSE 8080 9090
ENV HTTP_ADDR=0.0.0.0:8080 INTERNAL_ADDR=0.0.0.0:9090 APP_ENV=production
HEALTHCHECK --interval=15s --timeout=6s --start-period=30s --retries=3 CMD ["/usr/local/bin/previa-server", "healthcheck"]
ENTRYPOINT ["/usr/local/bin/previa-server"]

FROM ${RUNTIME_IMAGE} AS worker
COPY --from=build /out/bin/previa-worker /out/bin/previa-route-sync /out/bin/previa-housekeeping /usr/local/bin/
# Source workspaces and the route file live here (named volumes inherit the
# ownership, so the non-root worker can write them).
COPY --from=build --chown=65532:65532 /out/state /var/lib/previa
USER 65532:65532
EXPOSE 9091
ENV WORKER_INTERNAL_ADDR=0.0.0.0:9091 APP_ENV=production SOURCE_WORKSPACE_ROOT=/var/lib/previa/sources ROUTER_DYNAMIC_DIR=/var/lib/previa/router
HEALTHCHECK --interval=15s --timeout=6s --start-period=60s --retries=3 CMD ["/usr/local/bin/previa-worker", "healthcheck"]
ENTRYPOINT ["/usr/local/bin/previa-worker"]

# The egress proxy and prober the worker starts for builds and previews.
FROM scratch AS egress
COPY --from=build /out/bin/previa-egress /previa-egress
USER 65532:65532
EXPOSE 3128
ENTRYPOINT ["/previa-egress"]
CMD ["serve"]
