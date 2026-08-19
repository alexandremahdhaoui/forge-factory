#!/bin/sh
set -eu

# Every exported function must have a caller in production code. A ported
# function nobody calls still counts as ported, which is how half a panel goes
# missing while every gate reads green.
#
# A definition line matches the call pattern too, so the definitions are counted
# and subtracted. Without that every function is its own caller and the gate
# passes on anything.

# Generated code is a real caller, so it is searched. It is not a source of
# definitions, because its author is the generator.
CALLERS=$(find internal pkg cmd -name '*.go' ! -name '*_test.go' ! -path 'internal/mocks/*')
OWNED=$(echo "$CALLERS" | grep -v 'zz_generated')

fail=0

for file in $OWNED; do
    case "$file" in
        internal/types/*) continue ;;
    esac

    names=$(grep -oE '^func (\([^)]*\) )?[A-Z][A-Za-z0-9_]*\(' "$file" |
        sed 's/(.*//; s/^func //' | sort -u)

    for name in $names; do
        case "$name" in
            main|TestMain) continue ;;
        esac

        hits=$(grep -hoE "[^A-Za-z0-9_]${name}\(" $CALLERS | wc -l)
        defs=$(grep -hoE "^func (\([^)]*\) )?${name}\(" $CALLERS | wc -l)

        if [ $((hits - defs)) -le 0 ]; then
            echo "$file: $name has no caller in production code" >&2
            fail=1
        fi
    done
done

if [ "$fail" -eq 0 ]; then
    echo "every exported function has a production caller"
fi

exit "$fail"
