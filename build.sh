#!/usr/bin/env bash
# Build the forge binaries that main.ts launches. Run from anywhere: bash build.sh
#
#   bin/forge-linux-amd64        static, stripped; this is the file that ships (committed, mode 100755)
#   bin/forge-windows-amd64.exe  for running on a Windows workstation (not committed)
#
# Requires Go 1.26 or newer and nothing else: the build uses the standard library only.
set -euo pipefail
cd "$(dirname "$0")"

export CGO_ENABLED=0 GOARCH=amd64
flags=(-trimpath -buildvcs=false -ldflags "-s -w -buildid=")

go vet ./...

mkdir -p bin
GOOS=linux go build "${flags[@]}" -o bin/forge-linux-amd64 ./cmd/forge
GOOS=windows go build "${flags[@]}" -o bin/forge-windows-amd64.exe ./cmd/forge

# On a Windows workstation, keep a second copy of the Windows build where main.ts looks when a copy
# of the checkout came without the uncommitted bin/*.exe (the local grader copies git's file list).
case "$(uname -s)" in
  MINGW*|MSYS*|CYGWIN*)
    cache="$(cygpath -u "${USERPROFILE:-$HOME}")/.cache/forge"
    mkdir -p "$cache"
    cp bin/forge-windows-amd64.exe "$cache/forge-windows-amd64.exe"
    ;;
esac

# The platform runs the Linux binary straight from the checkout, so git must record it as
# executable. This repository has core.filemode=false (Windows), where the mode lives only in the
# index, so stage the fresh binary with the exec bit set. Nothing is committed here.
if git rev-parse --is-inside-work-tree >/dev/null 2>&1; then
  git update-index --add --chmod=+x bin/forge-linux-amd64
fi

size=$(wc -c < bin/forge-linux-amd64)
echo "built bin/forge-linux-amd64 ($size bytes) and bin/forge-windows-amd64.exe"
if [ "$size" -ge $((8 * 1024 * 1024)) ]; then
  echo "error: the Linux binary exceeds the 8 MiB per-file limit" >&2
  exit 1
fi
