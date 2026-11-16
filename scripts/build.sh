#!/bin/sh
set -eu
cd "$(dirname "$0")/.."
out=${1:-${MMX_TEST_ROOT:-$HOME/.cache/mmx-testing}/bin}
exec python3 tests/integration/testenv.py --root "$(dirname "$out")" -- \
  sh -eu -c '
out=$1
mkdir -p "$out"
CGO_ENABLED=0 go build -trimpath -o "$out/mmx" ./cmd/mmx
for arch in arm64 amd64; do
  CGO_ENABLED=0 GOOS=linux GOARCH=$arch go build -trimpath -ldflags="-s -w" -o "$out/mmx-runner-linux-$arch" ./cmd/mmx-runner
done
rm -rf "$out/official_extra_modules"
cp -R official_extra_modules "$out/official_extra_modules"
printf "Built controller and Linux runners in %s, beside the official extra modules\n" "$out"
' sh "$out"
