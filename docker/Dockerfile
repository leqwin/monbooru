# Stage 1: Build
FROM golang:1.25-bookworm AS builder
WORKDIR /src
COPY go.mod go.sum ./
RUN go mod download
COPY . .
# CGO_ENABLED=1 required for the tagger build tag (ONNX Runtime Go bindings use CGo)
RUN VERSION=$(cat VERSION.md 2>/dev/null | tr -d '[:space:]') && \
    CGO_ENABLED=1 go build -tags tagger \
    -ldflags="-s -w -X 'github.com/leqwin/monbooru/internal/web.Version=${VERSION}'" \
    -o /monbooru ./cmd/monbooru

# Stage 2: Runtime
FROM debian:bookworm-slim
RUN apt-get update && apt-get install -y --no-install-recommends \
    ffmpeg \
    ca-certificates \
    curl \
    && rm -rf /var/lib/apt/lists/*

# Download ONNX Runtime shared library needed by the autotagger at runtime.
# onnxruntime_go v1.19.0 requests ORT API version 21, which requires ORT 1.21.x.
ARG ORT_VERSION=1.21.0
RUN ARCH=$(dpkg --print-architecture) && \
    case "$ARCH" in \
      amd64) ONNX_ARCH="x64" ;; \
      arm64) ONNX_ARCH="aarch64" ;; \
      *) echo "Unsupported arch: $ARCH" && exit 1 ;; \
    esac && \
    curl -fsSL "https://github.com/microsoft/onnxruntime/releases/download/v${ORT_VERSION}/onnxruntime-linux-${ONNX_ARCH}-${ORT_VERSION}.tgz" \
      | tar -xz -C /tmp && \
    cp /tmp/onnxruntime-linux-${ONNX_ARCH}-${ORT_VERSION}/lib/libonnxruntime.so.${ORT_VERSION} /usr/lib/libonnxruntime.so && \
    ldconfig && \
    rm -rf /tmp/onnxruntime-*

COPY --from=builder /monbooru /usr/local/bin/monbooru
EXPOSE 8080
ENTRYPOINT ["/usr/local/bin/monbooru", "-config", "/config/monbooru.toml"]
