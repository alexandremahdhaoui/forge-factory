#!/bin/sh
set -eu

MODULE="github.com/alexandremahdhaoui/forge-factory"

fail=0

check() {
    layer="$1"
    banned="$2"
    reason="$3"

    for dir in $(find "$layer" -type d 2>/dev/null); do
        files=$(find "$dir" -maxdepth 1 -name '*.go' ! -name '*_test.go' 2>/dev/null)
        [ -n "$files" ] || continue

        for target in $banned; do
            hits=$(grep -l "$MODULE/$target" $files 2>/dev/null || true)

            if [ -n "$hits" ]; then
                echo "$hits imports $target. $reason" >&2
                fail=1
            fi
        done
    done
}

check internal/adapter "internal/controller internal/driver" \
    "An adapter talks to the outside world. It never knows who calls it."

check internal/controller "internal/driver" \
    "A controller holds business logic. Drivers call controllers, never the reverse."

check pkg/config "internal/adapter internal/controller internal/driver" \
    "The factory schema is plain data. It depends on nothing."

check internal/types "internal/adapter internal/controller internal/driver" \
    "Types are plain data. They depend on nothing."

for dir in $(find internal/adapter -mindepth 1 -maxdepth 1 -type d); do
    name=$(basename "$dir")
    siblings=$(find internal/adapter -mindepth 1 -maxdepth 1 -type d ! -name "$name" -exec basename {} \;)
    files=$(find "$dir" -maxdepth 1 -name '*.go' ! -name '*_test.go')

    for sibling in $siblings; do
        case "$sibling" in
            execadapter|fsadapter) continue ;;
        esac

        hits=$(grep -l "$MODULE/internal/adapter/$sibling" $files 2>/dev/null || true)

        if [ -n "$hits" ]; then
            echo "$hits imports the $sibling adapter. Adapters do not call each other." >&2
            fail=1
        fi
    done
done

if [ "$fail" -eq 0 ]; then
    echo "every layer depends only on the layers below it"
fi

exit "$fail"
