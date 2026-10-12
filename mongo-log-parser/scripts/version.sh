#!/usr/bin/env bash
# Prints the version string to stamp into a build, and nothing else.
#
# There are two answers, and which one applies depends on whether this tree is under version
# control. Under git, the nearest tag is the version, printed as it is: every release gets the next
# patch tag (v0.7.1, v0.7.2, ...), so nothing is appended after it. The repository holds pidba beside the parser, so the parser's tags are named
# for it — mongo-log-parser/v0.6.0 — and only those are read. Without git there is no history to name, so the version is the baseline below
# plus a fingerprint of the sources themselves — which is the figure that actually answers the
# question a version is asked for here: "is the tool I am running the current one, or something I
# built weeks ago and forgot about". Two builds of the same sources fingerprint identically, and any
# edit to any Go file changes it.
set -euo pipefail

root="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd -P)"

# BASELINE is the released version this tree descends from. Bump it when you cut a release, and tag
# it too (mongo-log-parser/vX.Y.Z); after that `git describe` takes over and this is the fallback for
# anyone building from a tarball or before the first tag.
#
# A release is a state worth going back to or handing to somebody, not every feature that lands. The
# minor goes up for one of those; the patch goes up for a state worth marking on the way to it. What
# this is not is a counter: 0.3.0 and 0.4.0 were once the same feature twenty-six minutes apart, one
# of them half-finished, and two numbers for that say less than one does.
BASELINE="0.7.1"

if git -C "$root" rev-parse --git-dir >/dev/null 2>&1; then
  described="$(git -C "$root" describe --tags --match 'mongo-log-parser/v*' --abbrev=0 2>/dev/null || true)"
  if [[ -n "$described" ]]; then
    printf '%s\n' "${described#mongo-log-parser/}"
    exit 0
  fi
fi

# A fingerprint of every tracked-by-intent source file: the Go code, the module files, and this
# script. Sorted, so it does not depend on the order the filesystem hands them back.
fingerprint="$(
  cd "$root"
  find . -type f \( -name '*.go' -o -name 'go.mod' -o -name 'go.sum' -o -name 'version.sh' \) \
    -not -path './.git/*' -print0 |
    sort -z |
    xargs -0 shasum -a 256 |
    shasum -a 256 |
    cut -c1-12
)"
printf '%s+%s\n' "$BASELINE" "$fingerprint"
