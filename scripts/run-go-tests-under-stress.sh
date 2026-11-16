#!/bin/sh
# Runs MMX's Go tests many times, several copies at once, to find a test that
# passes on an idle machine and fails on a loaded one. It builds each test
# package once with the race detector and runs the binaries with Go's own
# stress tool (golang.org/x/tools/cmd/stress), which keeps the output of
# every failed run. docs/changing-mmx/testing.md says when to run it.
#
# Usage, from the repository root:
#   scripts/run-go-tests-under-stress.sh [--runs N] [--runs-at-once N] [--tests-matching REGEXP]
#
#   --runs N                 how many runs in all; each run is one run of every
#                            test package (default 40)
#   --runs-at-once N         how many runs at the same time (default 4)
#   --tests-matching REGEXP  only the tests whose names match, as go test -run
#
# The exit code is 0 when every run passed and 1 when one failed. The
# failed tests and the files with their whole output are listed at the end;
# after runs that all passed, the binaries and logs are removed.
set -eu

runs=40
runs_at_once=4
tests_matching=.
while [ $# -gt 0 ]; do
	case "$1" in
	--runs) runs=$2; shift 2 ;;
	--runs-at-once) runs_at_once=$2; shift 2 ;;
	--tests-matching) tests_matching=$2; shift 2 ;;
	*) echo "unknown argument: $1" >&2; exit 2 ;;
	esac
done

cd "$(dirname "$0")/.."
repository=$(pwd)
work=$(mktemp -d "${TMPDIR:-/tmp}/mmx-go-tests-under-stress.XXXXXX")
GOBIN=$work go install golang.org/x/tools/cmd/stress@v0.48.0

# One run is every test package's binary in turn, each from its own
# directory, as go test runs it. A package that fails does not stop the
# run: the packages after it still run, so one failure hides no other.
run_once=$work/run_every_test_package_once.sh
echo '#!/bin/sh' > "$run_once"
echo 'failed=0' >> "$run_once"
packages=$(go list -f '{{if or .TestGoFiles .XTestGoFiles}}{{.Dir}}{{end}}' ./...)
matching_tests=0
for package in $packages; do
	binary=$work/$(echo "${package#"$repository"/}" | tr / _).test
	go test -c -race -o "$binary" "$package"
	matching_tests=$((matching_tests + $( (cd "$package" && "$binary" -test.list "$tests_matching") | grep -c '^Test' || true)))
	echo "(cd '$package' && '$binary' -test.count=1 -test.run '$tests_matching') || failed=1" >> "$run_once"
done
echo 'exit $failed' >> "$run_once"
chmod +x "$run_once"
# A name that matches no test would pass every run without testing anything.
if [ "$matching_tests" -eq 0 ]; then
	echo "no test matches $tests_matching" >&2
	rm -rf "$work"
	exit 2
fi
echo "$matching_tests tests, $runs runs, $runs_at_once at once"

# One run takes a few minutes on an idle machine; the time limit is far
# above that, so only a hang reaches it.
if "$work/stress" -p "$runs_at_once" -count "$runs" -timeout 30m -o "$work/failure-" "$run_once" > "$work/stress.log" 2>&1; then
	tail -n 1 "$work/stress.log"
	rm -rf "$work"
	exit 0
fi
tail -n 1 "$work/stress.log"
for failure in "$work"/failure-*; do
	[ -e "$failure" ] || continue
	echo "$failure:"
	grep -E -e '--- FAIL|_test\.go:[0-9]+: ' "$failure" | head -n 10
done
exit 1
