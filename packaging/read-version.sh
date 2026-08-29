#!/bin/sh
# packaging/read-version.sh

set -eu

version=$(tr -d '[:space:]' < VERSION.md)
if ! echo "$version" | grep -qE '^v[0-9]+\.[0-9]+\.[0-9]+$'; then
  echo "VERSION.md must be strict SemVer 'vMAJOR.MINOR.PATCH', got: $version" >&2
  exit 1
fi
echo "$version"
