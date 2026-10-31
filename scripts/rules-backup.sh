#!/usr/bin/env bash
# Export rule settings, including secrets, to a new private file.
set -euo pipefail
if [[ $# -ne 2 ]]; then
  echo "usage: $0 <config.yaml> <new-rules.yaml-or-json>" >&2
  exit 1
fi
exec "${MAILRELAY_BIN:-mailrelay}" -config "$1" -export-rules "$2"
