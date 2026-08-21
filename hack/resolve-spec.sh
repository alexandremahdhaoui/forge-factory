#!/bin/sh
# Resolve a shared spec from the workspace, falling back to the version the
# register carries for the module. Shared by every consumer repo; the
# canonical copy lives in golden-spec and the tool repos carry copies.
#
# Usage:  resolve-spec.sh <module-path> <spec-path-within-module> [dest-dir]
# Example: resolve-spec.sh github.com/alexandremahdhaoui/golden-spec \
#              api/golden.v1.yaml .forge/spec-cache
#
# Resolution order:
#   1. The sibling checkout named by the module's basename - the local
#      checkout wins, like any member.
#   2. The register's internal track for the module: the highest track's
#      current version, fetched from GitHub at that tag. The register
#      checkout is read when present; the index file itself is fetched
#      from the register repo's main branch when not.
#
# Writes the spec to <dest-dir>/<basename> and records provenance in
# <dest-dir>/.source so builds are auditable.

set -eu

MODULE="${1:?module path required}"
SPEC_REL="${2:?spec path within module required}"
DEST="${3:-.forge/spec-cache}"

die() { echo "resolve-spec: $*" >&2; exit 1; }

# --- locate the workspace root by walking up from the repo -------------------
find_workspace_root() {
    dir=$(pwd)
    while [ "$dir" != "/" ]; do
        if [ -f "$dir/forge-factory.yaml" ]; then
            printf '%s\n' "$dir"
            return 0
        fi
        dir=$(dirname "$dir")
    done
    # A lone checkout has no placed factory file; its parent is the best
    # guess at where siblings would live.
    dirname "$(pwd)"
}

WS_DIR=$(find_workspace_root)
NAME=$(basename "$MODULE")
OWNER_REPO=$(printf '%s\n' "$MODULE" | sed 's|^github.com/||')

mkdir -p "$DEST"
BASENAME=$(basename "$SPEC_REL")

# --- 1. sibling checkout by basename ----------------------------------------
if [ -d "$WS_DIR/$NAME" ]; then
    SRC="$WS_DIR/$NAME/$SPEC_REL"
    [ -f "$SRC" ] || die "$WS_DIR/$NAME is checked out but $SRC does not exist"

    cp "$SRC" "$DEST/$BASENAME"
    printf 'source=local\npath=%s\nresolved=%s\n' "$SRC" "$(date -u +%Y-%m-%dT%H:%M:%SZ)" \
        > "$DEST/.source"
    echo "resolve-spec: $MODULE -> local $SRC"
    exit 0
fi

# --- 2. the register's internal track ---------------------------------------
# The register catalogs internal modules under index/internal/<module>/, one
# file per track; the highest track's current is the version to fetch.
REGISTER_REPO="golden-register"
INDEX_REL="index/internal/$MODULE"

current_from_index_dir() {
    track_file=$(ls "$1"/*.json 2>/dev/null | sort -V | tail -1)
    [ -n "$track_file" ] || return 1
    sed -n 's/.*"current":"\([^"]*\)".*/\1/p' "$track_file"
}

VERSION=""
if [ -d "$WS_DIR/$REGISTER_REPO/$INDEX_REL" ]; then
    VERSION=$(current_from_index_dir "$WS_DIR/$REGISTER_REPO/$INDEX_REL") \
        || die "$MODULE has no track in the register checkout - publish it: forge-register publish internal:$MODULE <version> --provenance <revision>"
else
    # No register checkout: read the index from the register repo itself.
    # PUBLIC REPOS ONLY - raw.githubusercontent.com 404s on private repos.
    REG_OWNER=$(printf '%s\n' "$OWNER_REPO" | cut -d/ -f1)
    for TRACK_URL in $(printf 'https://api.github.com/repos/%s/%s/contents/%s\n' \
            "$REG_OWNER" "$REGISTER_REPO" "$INDEX_REL"); do
        VERSION=$(curl -fsSL "$TRACK_URL" 2>/dev/null \
            | sed -n 's/.*"download_url": *"\([^"]*\.json\)".*/\1/p' | sort -V | tail -1 \
            | xargs -r curl -fsSL 2>/dev/null \
            | sed -n 's/.*"current":"\([^"]*\)".*/\1/p') || true
    done
    [ -n "$VERSION" ] || die "$MODULE: no sibling checkout at $WS_DIR/$NAME and the register index is unreachable"
fi

[ -n "$VERSION" ] || die "$MODULE has no adoptable version in the register"

# 2a. Unauthenticated raw fetch. Fast, but PUBLIC REPOS ONLY.
URL="https://raw.githubusercontent.com/$OWNER_REPO/$VERSION/$SPEC_REL"

if curl -fsSL "$URL" -o "$DEST/$BASENAME" 2>/dev/null; then
    printf 'source=register-raw\nurl=%s\nversion=%s\nresolved=%s\n' \
        "$URL" "$VERSION" "$(date -u +%Y-%m-%dT%H:%M:%SZ)" > "$DEST/.source"
    echo "resolve-spec: $MODULE -> register $VERSION via $URL"
    exit 0
fi

# 2b. Shallow clone over SSH. Works for private repos using the same key git
#     already uses, so no token handling is needed anywhere.
TMP=$(mktemp -d)
trap 'rm -rf "$TMP"' EXIT

git clone --quiet --depth 1 --branch "$VERSION" \
    "git@github.com:$OWNER_REPO.git" "$TMP/repo" 2>/dev/null \
    || die "fetching $MODULE at $VERSION: not reachable via raw or ssh (is the tag pushed?)"

[ -f "$TMP/repo/$SPEC_REL" ] \
    || die "$SPEC_REL not found in $MODULE at $VERSION"

cp "$TMP/repo/$SPEC_REL" "$DEST/$BASENAME"
printf 'source=register-ssh\nrepo=git@github.com:%s.git\nversion=%s\nresolved=%s\n' \
    "$OWNER_REPO" "$VERSION" "$(date -u +%Y-%m-%dT%H:%M:%SZ)" > "$DEST/.source"
echo "resolve-spec: $MODULE -> register $VERSION via ssh $OWNER_REPO"
