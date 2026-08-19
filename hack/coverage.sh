#!/bin/sh
set -eu

FLOOR="${1:-90}"
OUT="${2:-tmp/coverage.out}"

mkdir -p "$(dirname "$OUT")"

PKGS=$(go list ./internal/... ./pkg/... | grep -v '/internal/mocks/' | paste -sd, -)

go test -tags "integration e2e" -coverpkg="$PKGS" -coverprofile="$OUT.raw" ./... >/dev/null

grep -v '/internal/mocks/' "$OUT.raw" > "$OUT"

TOTAL=$(go tool cover -func="$OUT" | awk '/^total:/ {gsub(/%/,"",$3); print $3}')

echo "coverage ${TOTAL}% against a floor of ${FLOOR}%"

awk -v total="$TOTAL" -v floor="$FLOOR" 'BEGIN { exit (total + 0 >= floor + 0) ? 0 : 1 }' || {
    echo "coverage ${TOTAL}% is below the floor of ${FLOOR}%" >&2
    go tool cover -func="$OUT" | awk '$3 != "100.0%"' >&2
    exit 1
}
