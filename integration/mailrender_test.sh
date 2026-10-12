#!/usr/bin/env bash
set -euo pipefail
exec xvfb-run -a -s '-screen 0 1600x1000x24 -nolisten tcp' /usr/bin/python3 "$(dirname "$0")/testdata/mailrender_contract.py" "$(dirname "$(realpath "$1")")/mailrender" "$2"
