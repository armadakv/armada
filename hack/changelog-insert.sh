#!/usr/bin/env bash
# Usage: changelog-insert.sh NOTES.md docs/changelog.md
# Inserts a generated release section above the previous ones. Idempotent: a version already present is left alone,
# and an "## Unreleased" section is replaced. Only the part above "## Earlier releases" is considered.
set -euo pipefail

notes=$1
file=$2
heading=$(head -n1 "$notes")
[[ $heading == "## "* ]] || { echo "$notes must start with a '## ' heading" >&2; exit 1; }
version=$(awk '{print $2}' <<<"$heading")

if [[ $version != Unreleased ]] &&
  sed '/^## Earlier releases/,$d' "$file" | awk -v v="$version" '$1 == "##" && $2 == v {f = 1} END {exit !f}'; then
  echo "$version is already in $file, nothing to do"
  exit 0
fi

tmp=$(mktemp)
trap 'rm -f "$tmp"' EXIT
awk -v notes="$notes" '
  /^## Earlier releases/ { tail = 1 }
  !tail && /^## Unreleased/ { skip = 1; next }
  /^## / { skip = 0 }
  skip { next }
  /^## / && !done {
    while ((getline l < notes) > 0) lines[n++] = l
    while (n > 0 && lines[n - 1] == "") n--
    for (i = 0; i < n; i++) print lines[i]
    print ""
    done = 1
  }
  { print }
' "$file" >"$tmp"
cat "$tmp" >"$file"
