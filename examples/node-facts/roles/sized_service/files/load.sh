#!/bin/sh
set -eu
cp /srv/mmx-facts-demo/config /srv/mmx-facts-demo/loaded
printf '%s\n' "$1" >> /srv/mmx-facts-demo/actions
