#!/usr/bin/env bash
set -euo pipefail

cd "$(dirname "${BASH_SOURCE[0]}")/.."

GOTOOLCHAIN="go$(cat .go-version)"
export GOTOOLCHAIN
go version
go env -w GOTOOLCHAIN="${GOTOOLCHAIN}"

AUTO_MODE=true make tools-setup
make tools-check