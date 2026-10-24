# The relay image. goreleaser (dockers_v2) builds the binaries and passes
# them in the build context as <os>/<arch>/seasoning. Locally:
#   GOOS=linux GOARCH=arm64 CGO_ENABLED=0 go build -o linux/arm64/seasoning ./cmd/seasoning
#   docker buildx build --platform linux/arm64 -f deploy/Dockerfile .
# The prep stage runs on the build machine's platform, so cross-building
# needs no emulation.
FROM --platform=$BUILDPLATFORM busybox:1.37 AS prep
RUN mkdir -p /data

FROM gcr.io/distroless/static-debian13:nonroot
ARG TARGETPLATFORM
COPY --from=prep --chown=65532:65532 /data /data
COPY $TARGETPLATFORM/seasoning /usr/local/bin/seasoning
VOLUME /data
EXPOSE 8090
# Needs SEASONING_RELAY_TOKEN. Behind a reverse proxy, add --trust-proxy so
# rate limits use the client address from X-Forwarded-For.
ENTRYPOINT ["/usr/local/bin/seasoning"]
CMD ["serve", "--listen", "0.0.0.0:8090", "--db", "/data/relay.db"]
