#!/usr/bin/env bash
set -euo pipefail

# Keep source, destination and inclusion policy in the documented TOML file.
project_dir=$(CDPATH= cd -- "$(dirname -- "${BASH_SOURCE[0]}")" && pwd)

# An optional first path selects another configuration; remaining args are
# sync overrides. Without a path, use the documented rxvt-unicode example.
config_file="$project_dir/example.toml"
if [[ $# -gt 0 && "$1" != -* ]]; then
    config_file=$1
    shift
fi

"$project_dir/build.sh"

# Sync initializes a new destination; later flags override TOML settings.
exec "$project_dir/cvsgitrelay" sync "$config_file" "$@"
