#!/bin/sh
set -eu

project_dir=$(CDPATH= cd -- "$(dirname -- "$0")" && pwd)

# Stop on build failure and preserve the caller's directory for relative args.
"$project_dir/build.sh"
exec "$project_dir/cvsgitrelay" "$@"
