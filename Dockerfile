# Previa release images (docs/25-production-deployment.md). One build stage
# compiles every command from source with generated templates and CSS; the
# runtime images hold only static binaries and run as UID 65532. Release
# builds always name their --target; without one the build yields the server.
#
#   docker build --target server -t previa-server .
#   docker build --target worker -t previa-worker .
#   docker build --target egress -t previa-egress .
#
# Base images are pinned by digest; Tailwind is pinned by checksum
# (deploy/install-tailwind.sh); Go modules by go.sum.
ARG GO_IMAGE=golang:1.26.10-bookworm@sha256:fe3da92a5cb2881e4aa75e625cb0b7c50571632fb4cf340cbe53e6ccdcffdd51
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
# The verified Tailwind binary is kept in a build cache, so repeated builds
# need not download it from GitHub again.
RUN --mount=type=cache,target=/go/pkg/mod --mount=type=cache,target=/root/.cache/go-build --mount=type=cache,target=/root/.cache/previa-tools <<BUILD
set -eu
sh deploy/install-tailwind.sh /root/.cache/previa-tools/tailwindcss
/root/.cache/previa-tools/tailwindcss -i web/styles/app.css -o web/static/css/app.css --minify
go tool -modfile=tools/go.mod templ generate -path web
mkdir -p /out/bin /out/state/sources /out/state/router /out/state/baselines
chmod 0700 /out/state/baselines
for command in server worker migrate route-sync housekeeping secrets-reencrypt egress; do
  GOOS="${TARGETOS}" GOARCH="${TARGETARCH:-$(go env GOARCH)}" go build -buildvcs=false -ldflags="-s -w -X github.com/ajaykathwatee/previa/internal/platform/version.Version=${VERSION}" \
    -o "/out/bin/previa-${command}" "./cmd/${command}"
done
BUILD

FROM ${RUNTIME_IMAGE} AS worker
COPY --from=build /out/bin/previa-worker /out/bin/previa-route-sync /out/bin/previa-housekeeping /usr/local/bin/
# Source workspaces, the route file and encrypted test-data baselines live
# here (named volumes inherit the ownership, so the non-root worker can write
# them).
COPY --from=build --chown=65532:65532 /out/state /var/lib/previa
USER 65532:65532
EXPOSE 9091
ENV WORKER_INTERNAL_ADDR=0.0.0.0:9091 APP_ENV=production SOURCE_WORKSPACE_ROOT=/var/lib/previa/sources ROUTER_DYNAMIC_DIR=/var/lib/previa/router PREVIEW_BASELINE_DIR=/var/lib/previa/baselines
HEALTHCHECK --interval=15s --timeout=6s --start-period=60s --retries=3 CMD ["/usr/local/bin/previa-worker", "healthcheck"]
ENTRYPOINT ["/usr/local/bin/previa-worker"]

# The egress proxy and prober the worker starts for builds and previews.
FROM scratch AS egress
COPY --from=build /out/bin/previa-egress /previa-egress
USER 65532:65532
EXPOSE 3128
ENTRYPOINT ["/previa-egress"]
CMD ["serve"]

# The dashboard is the last stage, so a build without --target (a Previa
# preview of this repository, see .previewrc.yaml) produces it.
FROM ${RUNTIME_IMAGE} AS server
COPY --from=build /out/bin/previa-server /out/bin/previa-migrate /out/bin/previa-secrets-reencrypt /usr/local/bin/
USER 65532:65532
EXPOSE 8080 9090
ENV HTTP_ADDR=0.0.0.0:8080 INTERNAL_ADDR=0.0.0.0:9090 APP_ENV=production
HEALTHCHECK --interval=15s --timeout=6s --start-period=30s --retries=3 CMD ["/usr/local/bin/previa-server", "healthcheck"]
ENTRYPOINT ["/usr/local/bin/previa-server"]
