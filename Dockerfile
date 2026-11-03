# Static bridge binary; the frontend build in internal/webui/dist is embedded at compile time.
# Cross-compiles on the build machine's platform, so multi-arch builds need no QEMU.
FROM --platform=$BUILDPLATFORM golang:1.27-alpine AS build
WORKDIR /src
COPY go.mod go.sum ./
RUN go mod download
COPY . .
ARG TARGETOS TARGETARCH VERSION=dev
RUN CGO_ENABLED=0 GOOS=$TARGETOS GOARCH=$TARGETARCH go build -trimpath -ldflags="-s -w -X main.version=${VERSION}" -o /bridge ./cmd/bridge

FROM gcr.io/distroless/static-debian12:nonroot
ARG VERSION=dev
LABEL org.opencontainers.image.source="https://github.com/spacecodee/spacee-herdr-bridge" \
      org.opencontainers.image.description="Bridge between the spacee-herdr web client and herdr-core" \
      org.opencontainers.image.licenses="MIT" \
      org.opencontainers.image.version="${VERSION}"
COPY --from=build /bridge /bridge
# Inside the container it listens on every interface; compose publishes it on host loopback only.
ENTRYPOINT ["/bridge", "-listen", "0.0.0.0:8080"]
