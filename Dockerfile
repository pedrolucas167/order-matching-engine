FROM --platform=$BUILDPLATFORM golang:1.27.3-alpine@sha256:b8c9b82affbbb82a020bbcf998dad2ba9516944ac7e883524642a481a9505e9c AS build
ARG TARGETOS
ARG TARGETARCH
WORKDIR /src
COPY go.mod go.sum ./
RUN go mod download && go mod verify
COPY cmd/order-server ./cmd/order-server
COPY internal ./internal
RUN CGO_ENABLED=0 GOOS=${TARGETOS} GOARCH=${TARGETARCH} go build -trimpath -ldflags="-s -w" -o /out/order-server ./cmd/order-server \
    && mkdir -p /out/data /out/licenses \
    && cp /usr/local/go/LICENSE /out/licenses/Go-LICENSE \
    && cp "$(go list -m -f '{{.Dir}}' github.com/google/uuid)/LICENSE" /out/licenses/uuid-LICENSE \
    && cp "$(go list -m -f '{{.Dir}}' go.etcd.io/bbolt)/LICENSE" /out/licenses/bbolt-LICENSE \
    && cp "$(go list -m -f '{{.Dir}}' golang.org/x/sys)/LICENSE" /out/licenses/x-sys-LICENSE

FROM scratch
ARG REVISION=unknown
LABEL org.opencontainers.image.title="Order Matching Engine"
LABEL org.opencontainers.image.description="Durable authenticated HTTP matching service for simulations"
LABEL org.opencontainers.image.source="https://github.com/pedrolucas167/order-matching-engine"
LABEL org.opencontainers.image.revision=${REVISION}
COPY --from=build --chown=65532:65532 /out/order-server /order-server
COPY --from=build --chown=65532:65532 /out/data /data
COPY --from=build /out/licenses /licenses
USER 65532:65532
EXPOSE 8080
VOLUME ["/data"]
HEALTHCHECK --interval=30s --timeout=3s --start-period=5s --retries=3 CMD ["/order-server", "healthcheck"]
ENTRYPOINT ["/order-server"]
CMD ["-listen", ":8080", "-config", "/config/engine.json", "-db", "/data/engine.db"]
