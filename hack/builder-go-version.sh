#!/bin/sh
# Read the exact Go version used by the Docker builder, not the module minimum.
set -eu
dockerfile=${1:-Dockerfile}
builder_count=$(awk 'toupper($1) == "FROM" && toupper($(NF-1)) == "AS" && $NF == "builder" { n++ } END { print n+0 }' "$dockerfile")
[ "$builder_count" -eq 1 ] || { echo 'Dockerfile must have exactly one builder stage' >&2; exit 1; }
version=$(sed -nE 's/^FROM( --platform=[^ ]+)? golang:([0-9]+\.[0-9]+\.[0-9]+)(-[^ ]+)? AS builder$/\2/p' "$dockerfile")
case "$version" in
  ''|*[!0-9.]*) echo 'Dockerfile must have one builder stage with a pinned Go version' >&2; exit 1 ;;
esac
printf '%s\n' "$version"
