# Stage 1: build the binary.
FROM golang:1.26-trixie AS build
WORKDIR /src
COPY go.mod go.sum ./
RUN go mod download
COPY . .
# CGO_ENABLED=1 is required for the tagger build tag: the onnxruntime_go bindings use cgo.
RUN VERSION=$(cat VERSION.md 2>/dev/null | tr -d '[:space:]') && \
    REPO=$(cat REPOSITORY.md 2>/dev/null | tr -d '[:space:]') && \
    DOC=$(cat DOC.md 2>/dev/null | tr -d '[:space:]') && \
    CGO_ENABLED=1 go build -tags tagger \
    -ldflags="-s -w -X 'github.com/monbooru/monbooru/internal/web.Version=${VERSION}' -X 'github.com/monbooru/monbooru/internal/web.RepoURL=${REPO}' -X 'github.com/monbooru/monbooru/internal/web.DocURL=${DOC}'" \
    -o /monbooru ./cmd/monbooru

# Stage 2: fetch runtime assets.
FROM debian:trixie-slim AS assets
RUN apt-get update && apt-get install -y --no-install-recommends \
      ca-certificates curl tar xz-utils \
    && rm -rf /var/lib/apt/lists/*

# Build a uid/gid 1000 "monbooru" account
COPY --from=gcr.io/distroless/cc-debian13:nonroot /etc/passwd /etc/group /idsrc/
RUN echo 'monbooru:x:1000:1000:monbooru:/home/monbooru:/sbin/nologin' >> /idsrc/passwd && \
    echo 'monbooru:x:1000:' >> /idsrc/group && \
    install -d -o 1000 -g 1000 /idsrc/home

# ONNX Runtime shared library for the autotagger. onnxruntime_go v1.19.0
# requests ORT API version 21, which requires ORT 1.21.x.
ARG ORT_VERSION=1.21.0
ARG ORT_SHA256_X64=7485c7e7aac6501b27e353dcbe068e45c61ab51fbaf598d13970dfae669d20bf
ARG ORT_SHA256_AARCH64=4508084bde1232ee1ab4b6fad2155be0ea2ccab1c1aae9910ddb3fb68a60805e
RUN ARCH=$(dpkg --print-architecture) && \
    case "$ARCH" in \
      amd64) ONNX_ARCH="x64";     ORT_SHA="${ORT_SHA256_X64}" ;; \
      arm64) ONNX_ARCH="aarch64"; ORT_SHA="${ORT_SHA256_AARCH64}" ;; \
      *) echo "Unsupported arch: $ARCH" && exit 1 ;; \
    esac && \
    curl -fsSL "https://github.com/microsoft/onnxruntime/releases/download/v${ORT_VERSION}/onnxruntime-linux-${ONNX_ARCH}-${ORT_VERSION}.tgz" \
      -o /tmp/ort.tgz && \
    echo "${ORT_SHA}  /tmp/ort.tgz" | sha256sum -c - && \
    tar -xzf /tmp/ort.tgz -C /tmp && \
    mkdir -p /opt/ort && \
    cp /tmp/onnxruntime-linux-${ONNX_ARCH}-${ORT_VERSION}/lib/libonnxruntime.so.${ORT_VERSION} /opt/ort/libonnxruntime.so && \
    rm -f /tmp/ort.tgz

# ffmpeg + ffprobe. BtbN's builds bundle their codecs statically but link
# glibc dynamically; the distroless cc base provides it.
ARG FFMPEG_RELEASE=n8.1
RUN ARCH=$(dpkg --print-architecture) && \
    case "$ARCH" in \
      amd64) FF_ARCH="linux64" ;; \
      arm64) FF_ARCH="linuxarm64" ;; \
      *) echo "Unsupported arch: $ARCH" && exit 1 ;; \
    esac && \
    curl -fsSL "https://github.com/BtbN/FFmpeg-Builds/releases/download/latest/ffmpeg-${FFMPEG_RELEASE}-latest-${FF_ARCH}-gpl-${FFMPEG_RELEASE#n}.tar.xz" -o /tmp/ffmpeg.tar.xz && \
    mkdir -p /tmp/ff /opt/ffmpeg && \
    tar -xJf /tmp/ffmpeg.tar.xz -C /tmp/ff --strip-components=1 && \
    cp /tmp/ff/bin/ffmpeg /tmp/ff/bin/ffprobe /opt/ffmpeg/

# Stage 3: runtime.
FROM gcr.io/distroless/cc-debian13:nonroot

COPY --from=assets /idsrc/passwd /etc/passwd
COPY --from=assets /idsrc/group  /etc/group
COPY --from=assets --chown=1000:1000 /idsrc/home /home/monbooru
COPY --from=assets /opt/ort/libonnxruntime.so       /usr/lib/libonnxruntime.so
COPY --from=assets /opt/ffmpeg/ffmpeg /opt/ffmpeg/ffprobe /usr/local/bin/
COPY --from=build  /monbooru                         /usr/local/bin/monbooru

ENV MONBOORU_SERVER_BIND_ADDRESS=0.0.0.0:8080
ENV HOME=/home/monbooru

EXPOSE 8080

HEALTHCHECK --interval=30s --timeout=5s --start-period=10s --retries=3 \
    CMD ["/usr/local/bin/monbooru", "healthcheck", "-config", "/config/monbooru.toml"]

USER 1000:1000

ENTRYPOINT ["/usr/local/bin/monbooru", "-config", "/config/monbooru.toml"]
