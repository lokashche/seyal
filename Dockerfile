# Seyal's container image: the same single binary as the release downloads, on a minimal base
# (no shell, no package manager, runs as a non-root user). Templates, styles and migrations are
# inside the binary; the sample processes and lists in examples/ are copied next to it.
#
#   docker build --build-arg VERSION=v0.1.0 -t seyal .
#
# The build stage runs on the builder's own platform and cross-compiles, so images for other
# platforms (e.g. arm64 servers) build quickly without emulation.

FROM --platform=$BUILDPLATFORM golang:1.26 AS build
WORKDIR /src
COPY go.mod go.sum ./
RUN go mod download
COPY . .
ARG VERSION=dev
ARG TARGETOS=linux
ARG TARGETARCH=amd64
RUN CGO_ENABLED=0 GOOS=$TARGETOS GOARCH=$TARGETARCH \
    go build -trimpath -ldflags "-s -w -X main.version=$VERSION" -o /out/seyal ./cmd/seyal \
 && mkdir -p /out/data/media

FROM gcr.io/distroless/static-debian12:nonroot
LABEL org.opencontainers.image.title="Seyal" \
      org.opencontainers.image.description="No forms. Just tell Seyal. An orchestrator with a ledger for small businesses." \
      org.opencontainers.image.licenses="AGPL-3.0-or-later"
WORKDIR /app
COPY --from=build /out/seyal /app/seyal
COPY examples /app/examples
# Voice notes and files people send live here; mount a volume on it (owned by the non-root user).
COPY --from=build --chown=65532:65532 /out/data /data
ENV SEYAL_ADDR=:8080 \
    SEYAL_MEDIA_DIR=/data/media
USER nonroot
EXPOSE 8080
ENTRYPOINT ["/app/seyal"]
CMD ["serve"]
