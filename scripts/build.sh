#!/usr/bin/env bash
# Builds a stamped mongo-parser. Takes the output path; defaults to bin/mongo-parser.
#
# `go build` on its own produces a binary that cannot say what it is, and one of those in a bin
# directory is indistinguishable from the current one. Every build that anybody runs should go
# through here.
set -euo pipefail

root="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd -P)"
out="${1:-$root/bin/mongo-parser}"
version="$("$root/scripts/version.sh")"
built_at="$(date -u +%Y-%m-%dT%H:%M:%SZ)"
package="github.com/ynsuy/mongo-parser/internal/cli"

mkdir -p "$(dirname "$out")"
go -C "$root" build \
  -ldflags "-X $package.Version=$version -X $package.BuiltAt=$built_at" \
  -o "$out" ./cmd/mongo-parser
printf '%s %s (%s)\n' "$out" "$version" "$built_at"
