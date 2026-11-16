#!/bin/bash
set -eu
load_config() {
  printf '%s: %s\n' "$WORKER_LABEL" "$(cat message)" > loaded
  printf 'load\n' >> actions
}
trap load_config HUP
load_config
while true; do
  sleep 1 &
  wait "$!" || true
done
