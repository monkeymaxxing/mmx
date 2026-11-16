#!/bin/sh
set -eu
cat /etc/mmx-config-demo/*.conf > /srv/mmx-config-demo/loaded
printf 'loaded\n' >> /srv/mmx-config-demo/actions
