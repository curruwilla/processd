#!/bin/sh
# The package ships the binary and the examples only. `processd setup` owns the
# runtime state: it writes the configuration, generates the API token and
# installs a systemd unit pointing at this binary.
set -eu

if [ ! -f /etc/processd/processd.yaml ]; then
	echo "processd installed. Run 'sudo processd setup' to finish the node:"
	echo "it writes /etc/processd/processd.yaml, generates the API token and"
	echo "installs and starts the systemd unit."
	echo "Examples: /usr/share/doc/processd/examples/"
fi
