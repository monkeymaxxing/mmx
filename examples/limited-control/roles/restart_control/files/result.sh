#!/bin/sh
set -eu
# Both values come from systemd or the fixed unit command, never a request.
printf '%s %s\n' "$INVOCATION_ID" "${1:-$SERVICE_RESULT}" > /run/mmx-demo-control/result.new
chmod 0644 /run/mmx-demo-control/result.new
mv /run/mmx-demo-control/result.new /run/mmx-demo-control/result
