#!/usr/bin/env bash
set -euo pipefail

# Build from the checkout while resolving the mirror relative to the caller.
project_dir=$(CDPATH= cd -- "$(dirname -- "${BASH_SOURCE[0]}")" && pwd)
repo=./git-rxvt-unicode

"$project_dir/build.sh"

# Initialize once; the explicit host/path separator is equivalent to the
# advertised CVSROOT and is required by the exporter's locator parser.
if [[ ! -f "$repo/cvsgitrelay.json" ]]; then
    "$project_dir/cvsgitrelay" init \
        --cvsroot ':pserver:anonymous@cvs.schmorp.de:/schmorpforge' \
        --module rxvt-unicode \
        --repo "$repo"
fi

# Forward optional sync flags, preserving quoting, signals and exit status.
exec "$project_dir/cvsgitrelay" sync --repo "$repo" "$@"
