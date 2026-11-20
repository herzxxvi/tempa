#!/usr/bin/env bash
# Build the forge binaries that main.ts launches. Run from anywhere: bash build.sh
#
#   bin/forge-linux-amd64        static, stripped; this is the file that ships (committed, mode 100755)
#   bin/forge-windows-amd64.exe  for running on a Windows workstation (not committed)
#
# Requires Go 1.26 or newer and nothing else: the build uses the standard library only.
set -euo pipefail
cd "$(dirname "$0")"

# CGO off: no libc, so the binary is static, runs on any Linux and resolves host names with Go's
# own resolver (/etc/hosts, then DNS). The netgo and osusergo tags say the same thing explicitly.
export CGO_ENABLED=0 GOARCH=amd64
flags=(-trimpath -buildvcs=false -tags netgo,osusergo -ldflags "-s -w -buildid=")

go vet ./...

mkdir -p bin
GOOS=linux go build "${flags[@]}" -o bin/forge-linux-amd64 ./cmd/forge
GOOS=windows go build "${flags[@]}" -o bin/forge-windows-amd64.exe ./cmd/forge

# The Linux binary must not need a dynamic loader, and must fit the 8 MiB per-file limit.
if grep -q 'ld-linux' bin/forge-linux-amd64; then
  echo "error: bin/forge-linux-amd64 is dynamically linked" >&2
  exit 1
fi
size=$(wc -c < bin/forge-linux-amd64)
if [ "$size" -ge $((8 * 1024 * 1024)) ]; then
  echo "error: the Linux binary exceeds the 8 MiB per-file limit" >&2
  exit 1
fi

# Development convenience, Windows workstations only: bin/*.exe is not committed, so a copy of the
# checkout made from git's file list has no Windows build in it.
# main.ts looks for it in the per-user cache directory in that case; keep that copy current.
# (To run any other build: FORGE_BIN=/path/to/binary node main.ts)
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

echo "built bin/forge-linux-amd64 ($size bytes, static) and bin/forge-windows-amd64.exe"
