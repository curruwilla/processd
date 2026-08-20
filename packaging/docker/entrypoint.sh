#!/bin/sh
# Bootstraps the node on the first start. Without a configuration and a token
# the daemon rejects every request, so the image would be useless out of the
# box. Mount /etc/processd to keep the token and the workers across restarts.
set -eu

CONFIG=/etc/processd/processd.yaml

if [ ! -f "$CONFIG" ]; then
	processd setup \
		--systemd=false \
		--start=false \
		--listen "${PROCESSD_LISTEN:-0.0.0.0:7373}"
fi

exec processd --config "$CONFIG" "$@"
