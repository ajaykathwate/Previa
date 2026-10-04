# The build egress proxy: one static binary, no shell, non-root. Build it with
# `make build-egress-image`, which compiles the binary first.
FROM scratch
COPY build-egress /build-egress
USER 65532:65532
EXPOSE 3128
ENTRYPOINT ["/build-egress"]
