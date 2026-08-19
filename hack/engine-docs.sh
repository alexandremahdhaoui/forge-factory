#!/bin/sh
set -eu

BIN=$(mktemp -d)
trap 'rm -rf "$BIN"' EXIT

fail=0

for dir in cmd/factory-lang-*; do
    name=$(basename "$dir")

    for required in docs/list.yaml docs/usage.md docs/schema.md; do
        if [ ! -f "$dir/$required" ]; then
            echo "$name is missing $required. forge's engine docs contract requires it." >&2
            fail=1
        fi
    done
done

[ "$fail" -eq 0 ] || exit 1

go build -o "$BIN/" ./cmd/... >/dev/null

for dir in cmd/factory-lang-*; do
    name=$(basename "$dir")

    if ! "$BIN/$name" docs validate >/dev/null 2>&1; then
        echo "$name failed its own docs validate" >&2
        "$BIN/$name" docs validate >&2 || true
        fail=1
        continue
    fi

    listed=$("$BIN/$name" docs list 2>/dev/null | wc -l)

    if [ "$listed" -lt 2 ]; then
        echo "$name lists $listed docs, want usage and schema" >&2
        fail=1
    fi
done

if [ "$fail" -eq 0 ]; then
    echo "every engine ships usage and schema, and passes its own docs validate"
fi

exit "$fail"
