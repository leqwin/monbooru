#!/bin/sh
# packaging/build-binaries.sh <lite|bundled>
#
# Assembles the downloadable archives into dist/. 
set -eu

shape=${1:?usage: build-binaries.sh lite|bundled}
. ./packaging/release-env.sh
mkdir -p dist stage

build() {
  profile=$1
  os=$2
  arch=$3

  exe=monbooru
  pkg=tarball
  gui=""
  desktop=false
  if [ "$profile" = desktop ]; then
    desktop=true
  fi
  if [ "$os" = windows ]; then
    exe=monbooru.exe
    pkg=zip
    if [ "$profile" = desktop ]; then
      gui="-H windowsgui"
    fi
  fi

  name="monbooru_${VERSION#v}_${profile}_${shape}_${os}_${arch}"
  dir="stage/$name"
  rm -rf "$dir"
  mkdir -p "$dir"

  compile "$pkg" "$dir/$exe"
  if [ "$shape" = bundled ]; then
    cp tools/* "$dir/"
  fi

  cp LICENSE README.md "$dir/"
  if [ "$os" = windows ]; then
    cp packaging/icons/monbooru.ico "$dir/"
    (cd stage && zip -qr "../dist/$name.zip" "$name")
    if [ "$profile" = desktop ]; then
      rm -rf "$dir-setup"
      cp -r "$dir" "$dir-setup"
      compile installer "$dir-setup/$exe"
    fi
  else
    tar -czf "dist/$name.tar.gz" -C stage "$name"
  fi
}

compile() {
  ldflags="-s -w $gui $LDFLAGS -X '$MOD.Package=$1' -X 'main.defaultDesktop=$desktop'"
  if [ "$shape" = bundled ]; then
    GOOS=$os GOARCH=$arch CGO_ENABLED=1 go build -tags tagger -trimpath \
      -ldflags="$ldflags" -o "$2" ./cmd/monbooru
  else
    GOOS=$os GOARCH=$arch CGO_ENABLED=0 go build -trimpath \
      -ldflags="$ldflags" -o "$2" ./cmd/monbooru
  fi
}

case "$shape" in
lite)
  for target in linux/amd64 linux/arm64 windows/amd64; do
    for profile in desktop server; do
      build "$profile" "${target%/*}" "${target#*/}"
    done
  done
  ;;
bundled)
  for profile in desktop server; do
    build "$profile" "${GOOS:?GOOS must name the target}" "${GOARCH:?GOARCH must name the target}"
  done
  ;;
*)
  echo "unknown shape $shape" >&2
  exit 2
  ;;
esac
ls -la dist
