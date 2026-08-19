#!/bin/sh
set -eu

TMP=$(mktemp -d)
trap 'rm -rf "$TMP"' EXIT

cp go.mod go.sum "$TMP/"

if ! GOWORK=off GOFLAGS= go build ./... >"$TMP/build.log" 2>&1; then
    echo "this repo does not build as a lone checkout" >&2
    echo "forge-factory is what materialises go.work, so it is the one repo that must" >&2
    echo "build without one. Everything else is built from a synced workspace." >&2
    head -10 "$TMP/build.log" >&2
    exit 1
fi

GOWORK=off go mod tidy >/dev/null 2>&1

if ! cmp -s go.mod "$TMP/go.mod" || ! cmp -s go.sum "$TMP/go.sum"; then
    diff -u "$TMP/go.mod" go.mod >&2 || true
    diff -u "$TMP/go.sum" go.sum >&2 || true
    cp "$TMP/go.mod" go.mod
    cp "$TMP/go.sum" go.sum
    echo "go.mod or go.sum is not tidy. run: GOWORK=off go mod tidy" >&2
    exit 1
fi

echo "builds and resolves as a lone checkout, and go.mod and go.sum are tidy"
