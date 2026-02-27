#!/usr/bin/env bash
set -euo pipefail

ROOT="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"
OUT_DIR="${OUT_DIR:-$ROOT/dist}"
mkdir -p "$OUT_DIR"

targets=(
  "darwin amd64"
  "darwin arm64"
  "linux amd64"
  "linux arm64"
)

for target in "${targets[@]}"; do
  read -r GOOS GOARCH <<<"$target"
  out="$OUT_DIR/threadpilot-${GOOS}-${GOARCH}"
  echo "Building $out"
  GOOS="$GOOS" GOARCH="$GOARCH" CGO_ENABLED=0 \
    go build -trimpath -ldflags="-s -w" -o "$out" ./cmd/threadpilot
  chmod +x "$out"
done

( cd "$OUT_DIR" && shasum -a 256 threadpilot-* > SHA256SUMS )

echo "Built artifacts in $OUT_DIR"
