#!/bin/sh
# Resolve an OpenAPI spec from the workspace, falling back to a pinned remote
# tag. Shared by every consumer repo; kept in golden-spec and copied in by the
# resolve-spec forge build step.
#
# Usage:  resolve-spec.sh <module-path> <spec-path-within-module> [dest-dir]
# Example: resolve-spec.sh github.com/alexandremahdhaoui/golden-spec \
#              api/golden.v1.yaml .forge/spec-cache
#
# Resolution order:
#   1. modules.<module>.path in workspace.yaml, if that directory exists
#   2. GitHub raw at modules.<module>.version tag
#
# Writes the spec to <dest-dir>/<basename> and records provenance in
# <dest-dir>/.source so builds are auditable.

set -eu

MODULE="${1:?module path required}"
SPEC_REL="${2:?spec path within module required}"
DEST="${3:-.forge/spec-cache}"

die() { echo "resolve-spec: $*" >&2; exit 1; }

# --- locate workspace.yaml by walking up from the repo root ------------------
find_workspace_file() {
    dir=$(pwd)
    while [ "$dir" != "/" ]; do
        if [ -f "$dir/workspace.yaml" ]; then
            printf '%s\n' "$dir/workspace.yaml"
            return 0
        fi
        dir=$(dirname "$dir")
    done
    return 1
}

WS_FILE=$(find_workspace_file) || die "no workspace.yaml found walking up from $(pwd)"
WS_DIR=$(dirname "$WS_FILE")

# --- read a scalar key from the module's block ------------------------------
# Deliberately avoids a yq dependency: the file is ours and its shape is fixed.
ws_get() {
    key="$1"
    awk -v mod="$MODULE" -v key="$key" '
        $0 ~ "^  " mod ":[[:space:]]*$" { inblock = 1; next }
        inblock && /^  [^ ]/            { inblock = 0 }
        inblock && $1 == key ":"        { print $2; exit }
        inblock {
            # handle "key: value" where awk splits on the colon-suffixed field
            if ($1 == key":") { print $2; exit }
        }
    ' "$WS_FILE"
}

LOCAL_PATH=$(ws_get "path")
VERSION=$(ws_get "version")

mkdir -p "$DEST"
BASENAME=$(basename "$SPEC_REL")

# --- 1. local workspace checkout --------------------------------------------
if [ -n "$LOCAL_PATH" ] && [ -d "$WS_DIR/$LOCAL_PATH" ]; then
    SRC="$WS_DIR/$LOCAL_PATH/$SPEC_REL"
    [ -f "$SRC" ] || die "workspace.yaml maps $MODULE to $LOCAL_PATH but $SRC does not exist"

    cp "$SRC" "$DEST/$BASENAME"
    printf 'source=local\npath=%s\nresolved=%s\n' "$SRC" "$(date -u +%Y-%m-%dT%H:%M:%SZ)" \
        > "$DEST/.source"
    echo "resolve-spec: $MODULE -> local $SRC"
    exit 0
fi

# --- 2. pinned remote tag ---------------------------------------------------
[ -n "$VERSION" ] || die "$MODULE has no local path and no version tag in workspace.yaml"

OWNER_REPO=$(printf '%s\n' "$MODULE" | sed 's|^github.com/||')

# 2a. Unauthenticated raw fetch. Fast, but PUBLIC REPOS ONLY - raw.github
#     usercontent.com returns 404 for private repos regardless of SSH access.
URL="https://raw.githubusercontent.com/$OWNER_REPO/$VERSION/$SPEC_REL"

if curl -fsSL "$URL" -o "$DEST/$BASENAME" 2>/dev/null; then
    printf 'source=remote-raw\nurl=%s\nversion=%s\nresolved=%s\n' \
        "$URL" "$VERSION" "$(date -u +%Y-%m-%dT%H:%M:%SZ)" > "$DEST/.source"
    echo "resolve-spec: $MODULE -> remote $URL"
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
printf 'source=remote-ssh\nrepo=git@github.com:%s.git\nversion=%s\nresolved=%s\n' \
    "$OWNER_REPO" "$VERSION" "$(date -u +%Y-%m-%dT%H:%M:%SZ)" > "$DEST/.source"
echo "resolve-spec: $MODULE -> remote ssh $OWNER_REPO@$VERSION"
