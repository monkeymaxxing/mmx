#!/bin/sh
set -eu
cd "$(dirname "$0")/.."
repository_directory=$PWD
# The analysis tools are built with the toolchain that go.mod pins, so that
# they can read code written for it.
GOTOOLCHAIN=$(go env GOVERSION)
export GOTOOLCHAIN
analysis_directory=$(mktemp -d "${TMPDIR:-/tmp}/mmx-static-analysis.XXXXXX")
trap 'rm -rf "$analysis_directory"' EXIT
trap 'exit 1' HUP INT TERM
export GOBIN="$analysis_directory/bin"
export STATICCHECK_CACHE="$analysis_directory/staticcheck-cache"

analysis_failed=0
run_analysis() {
  printf '\n== %s ==\n' "$*"
  if "$@"; then
    printf 'No findings.\n'
  else
    analysis_failed=1
  fi
}

install_analysis_tool() {
  printf '\n== Install %s ==\n' "$1"
  if ! go install "$1"; then
    analysis_failed=1
  fi
}

install_analysis_tool honnef.co/go/tools/cmd/staticcheck@v0.8.2
install_analysis_tool github.com/kisielk/errcheck@v1.30.0
install_analysis_tool github.com/nishanths/exhaustive/cmd/exhaustive@v0.15.0

# NilAway is built from its pinned source with one patch applied; the patch
# file says what it fixes and when it goes away.
build_patched_nilaway() {
  nilaway_module=go.uber.org/nilaway@v0.0.0-20260918162853-acb8859b9031
  nilaway_source=$(go mod download -json "$nilaway_module" | grep '"Dir"' | sed 's/.*"Dir": "//; s/",*$//')
  [ -n "$nilaway_source" ] || return 1
  cp -R "$nilaway_source" "$analysis_directory/nilaway-source" || return 1
  chmod -R u+w "$analysis_directory/nilaway-source"
  patch -p1 -d "$analysis_directory/nilaway-source" < "$repository_directory/scripts/nilaway-call-through-function-variable.patch" || return 1
  (cd "$analysis_directory/nilaway-source" && go build -o "$GOBIN/nilaway" ./cmd/nilaway)
}
printf '\n== Build NilAway from %s with scripts/nilaway-call-through-function-variable.patch ==\n' go.uber.org/nilaway@v0.0.0-20260918162853-acb8859b9031
if ! build_patched_nilaway; then
  analysis_failed=1
fi

run_analysis go vet ./...
run_analysis "$GOBIN/staticcheck" ./...
run_analysis "$GOBIN/errcheck" -ignoretests -exclude errcheck_excluded_functions.txt ./...
# exhaustive v0.15.0 uses -d to treat a default branch as exhaustive.
run_analysis "$GOBIN/exhaustive" -d ./...
run_analysis "$GOBIN/nilaway" -test=false -include-pkgs=mmx ./...

run_analysis python3 scripts/check-dependency-vulnerabilities.py
exit "$analysis_failed"
