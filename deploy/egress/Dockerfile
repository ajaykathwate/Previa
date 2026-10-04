# Previa's egress proxy and readiness prober: one static binary, no shell,
# non-root. Build it with `make egress-image`, which compiles the binary first.
FROM scratch
COPY previa-egress /previa-egress
USER 65532:65532
EXPOSE 3128
ENTRYPOINT ["/previa-egress"]
CMD ["serve"]
