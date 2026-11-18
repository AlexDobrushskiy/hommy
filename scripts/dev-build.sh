#!/bin/bash
# Builds bin/hommy and bin/hommy-e2e from this checkout.
set -euo pipefail
cd "$(dirname "$0")/.."
mkdir -p bin
go build -o bin/hommy ./cmd/hommy
go build -o bin/hommy-e2e ./cmd/hommy-e2e
echo "built bin/hommy bin/hommy-e2e"
