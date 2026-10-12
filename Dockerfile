FROM --platform=$BUILDPLATFORM golang:1.26-alpine AS build
ARG TARGETOS TARGETARCH
ARG VERSION=dev
ARG SOURCE_URL=
WORKDIR /src
COPY go.mod go.sum ./
RUN go mod download
COPY . .
RUN CGO_ENABLED=0 GOOS=$TARGETOS GOARCH=$TARGETARCH \
    go build -trimpath -ldflags="-s -w -X main.version=${VERSION} -X main.sourceURL=${SOURCE_URL}" -o /out/drawbridge ./cmd/drawbridge

# Runs as root: the mounted Docker socket is only writable by root or the
# host's docker group, whose ID differs between hosts.
FROM gcr.io/distroless/static-debian12
ARG VERSION=dev
ARG SOURCE_URL=
LABEL org.opencontainers.image.title="WAN Drawbridge" \
      org.opencontainers.image.description="Stops chosen Docker containers while your WAN is on a backup ISP" \
      org.opencontainers.image.version="${VERSION}" \
      org.opencontainers.image.source="${SOURCE_URL}" \
      org.opencontainers.image.licenses="AGPL-3.0-or-later"
COPY --from=build /out/drawbridge /drawbridge
VOLUME ["/data/db", "/data/cache"]
EXPOSE 8080
HEALTHCHECK --interval=30s --timeout=5s --start-period=20s --retries=3 CMD ["/drawbridge", "healthcheck"]
ENTRYPOINT ["/drawbridge"]
