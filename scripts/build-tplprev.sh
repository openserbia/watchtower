#!/bin/bash
set -euo pipefail

cd "$(git rev-parse --show-toplevel)"

exec task tplprev
