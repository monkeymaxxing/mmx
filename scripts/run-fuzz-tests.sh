#!/bin/sh
set -eu
cd "$(dirname "$0")/.."
trap 'exit 1' HUP INT TERM

print_usage() {
  cat <<'USAGE'
Usage: scripts/run-fuzz-tests.sh [--fuzz-time-per-target DURATION] [--fuzz-workers COUNT]

Run MMX's nine fuzz targets sequentially from the repository root.

  --fuzz-time-per-target DURATION  Go -fuzztime value per target (default: 10m)
  --fuzz-workers COUNT             Go -parallel value per target (default: 4)
  --help                           Print this help and exit

The script continues after a target fails and exits non-zero if any target failed.
USAGE
}

fuzz_time_per_target=10m
fuzz_workers=4
while [ "$#" -gt 0 ]; do
  case "$1" in
    --fuzz-time-per-target)
      if [ "$#" -lt 2 ]; then
        print_usage >&2
        exit 2
      fi
      fuzz_time_per_target=$2
      shift 2
      ;;
    --fuzz-workers)
      if [ "$#" -lt 2 ]; then
        print_usage >&2
        exit 2
      fi
      fuzz_workers=$2
      shift 2
      ;;
    --help)
      print_usage
      exit 0
      ;;
    *)
      print_usage >&2
      exit 2
      ;;
  esac
done

fuzz_failed=0
while read -r package_path fuzz_target; do
  printf '\n== Fuzz %s in %s ==\n' "$fuzz_target" "$package_path"
  if go test "$package_path" \
    -run='^$' \
    -fuzz="^${fuzz_target}$" \
    -fuzztime="$fuzz_time_per_target" \
    -parallel="$fuzz_workers"; then
    printf 'Passed: %s in %s\n' "$fuzz_target" "$package_path"
  else
    printf 'Failed: %s in %s\n' "$fuzz_target" "$package_path"
    fuzz_failed=1
  fi
done <<'FUZZ_TARGETS'
./internal/config FuzzProjectSettingsFile
./internal/config FuzzPlaybookFile
./internal/config FuzzInventoryFile
./internal/config FuzzRoleFile
./internal/secrets FuzzSecretsVaultPlaintext
./internal/secrets FuzzEncryptedSecretsVaultFile
./internal/moduleio FuzzExecutableModuleMessages
./internal/modules FuzzModuleDescriptionFile
./internal/endpoint FuzzNodeHTTPSAPIRequest
FUZZ_TARGETS
exit "$fuzz_failed"
