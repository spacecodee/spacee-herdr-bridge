# Static bridge binary; the frontend build in internal/webui/dist is embedded at compile time.
FROM golang:1.27-alpine AS build
WORKDIR /src
COPY go.mod go.sum ./
RUN go mod download
COPY . .
RUN CGO_ENABLED=0 go build -trimpath -ldflags="-s -w" -o /bridge ./cmd/bridge

FROM gcr.io/distroless/static-debian12:nonroot
COPY --from=build /bridge /bridge
# Inside the container it listens on every interface; compose publishes it on host loopback only.
ENTRYPOINT ["/bridge", "-listen", "0.0.0.0:8080"]
