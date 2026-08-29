#!/bin/sh
# packaging/fetch-tools.sh
#
# Downloads the bundled tools for one target into tools/
set -eu

ort_arch=""
ff_arch=""
ort_sha=""
ff_sha=""
case "${GOOS}/${GOARCH}" in
  linux/amd64)   ort_arch=x64;      ff_arch=linux64;    ort_sha=$ORT_SHA256_LINUX_X64;     ff_sha=$FFMPEG_SHA256_LINUX64 ;;
  linux/arm64)   ort_arch=aarch64;  ff_arch=linuxarm64; ort_sha=$ORT_SHA256_LINUX_AARCH64; ff_sha=$FFMPEG_SHA256_LINUXARM64 ;;
  windows/amd64) ort_arch=win-x64;  ff_arch=win64;      ort_sha=$ORT_SHA256_WIN_X64;       ff_sha=$FFMPEG_SHA256_WIN64 ;;
  *) echo "no bundled tools published for ${GOOS}/${GOARCH}" >&2; exit 1 ;;
esac

verify() {
  echo "$2  $1" | sha256sum -c -
}

mkdir -p tools
ort_base="https://github.com/microsoft/onnxruntime/releases/download/v${ORT_VERSION}"
ff_base="https://github.com/monbooru/ffmpeg-builds/releases/download/${FFMPEG_BUILD}"

if [ "$GOOS" = windows ]; then
  rm -rf /tmp/ort /tmp/ff
  curl -fsSL -o /tmp/ort.zip "${ort_base}/onnxruntime-${ort_arch}-${ORT_VERSION}.zip"
  verify /tmp/ort.zip "$ort_sha"
  unzip -q /tmp/ort.zip -d /tmp/ort
  cp "/tmp/ort/onnxruntime-${ort_arch}-${ORT_VERSION}/lib/onnxruntime.dll" tools/
  curl -fsSL -o /tmp/ffmpeg.zip "${ff_base}/${FFMPEG_NAME}-${ff_arch}.zip"
  verify /tmp/ffmpeg.zip "$ff_sha"
  unzip -q /tmp/ffmpeg.zip -d /tmp/ff
  cp "/tmp/ff/${FFMPEG_NAME}-${ff_arch}/bin/ffmpeg.exe" \
     "/tmp/ff/${FFMPEG_NAME}-${ff_arch}/bin/ffprobe.exe" tools/
else
  curl -fsSL -o /tmp/ort.tgz "${ort_base}/onnxruntime-linux-${ort_arch}-${ORT_VERSION}.tgz"
  verify /tmp/ort.tgz "$ort_sha"
  tar -xzf /tmp/ort.tgz -C /tmp
  cp "/tmp/onnxruntime-linux-${ort_arch}-${ORT_VERSION}/lib/libonnxruntime.so.${ORT_VERSION}" tools/libonnxruntime.so
  curl -fsSL -o /tmp/ffmpeg.tar.xz "${ff_base}/${FFMPEG_NAME}-${ff_arch}.tar.xz"
  verify /tmp/ffmpeg.tar.xz "$ff_sha"
  tar -xJf /tmp/ffmpeg.tar.xz -C /tmp
  cp "/tmp/${FFMPEG_NAME}-${ff_arch}/bin/ffmpeg" \
     "/tmp/${FFMPEG_NAME}-${ff_arch}/bin/ffprobe" tools/
fi
ls -la tools
