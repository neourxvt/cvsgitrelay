#!/bin/sh
set -eu

# Locate the checkout so builds also work when invoked from another directory.
project_dir=$(CDPATH= cd -- "$(dirname -- "$0")" && pwd)
cd "$project_dir"

exec go build -o "$project_dir/cvsgitrelay" ./cmd/cvsgitrelay
