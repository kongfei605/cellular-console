#!/bin/sh
set -eu
if [ "$(uname -s)" != Darwin ]; then
    echo 'This launcher requires macOS.' >&2
    exit 1
fi
script_dir=$(CDPATH= cd -- "$(dirname -- "$0")" && pwd)
project_dir=$(dirname -- "$script_dir")
cd "$project_dir"
make build-mac
credentials_path="$HOME/Library/Application Support/Cellular Console/credentials.json"
if [ ! -f "$credentials_path" ]; then
    python3 scripts/init_credentials.py --output "$credentials_path"
fi
exec "$project_dir/bin/ec20-console" -credentials "$credentials_path" "$@"
