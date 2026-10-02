#!/usr/bin/env bash
# Create a new goose migration in internal/store/migrations.
# Usage: make migrate-new name=add_foo
set -euo pipefail

name="${1:-}"
if [[ -z "$name" ]]; then
	echo "usage: make migrate-new name=add_foo" >&2
	exit 2
fi

if [[ ! "$name" =~ ^[a-z0-9]+(_[a-z0-9]+)*$ ]]; then
	echo "migration name must be snake_case, got: $name" >&2
	exit 2
fi

dir="internal/store/migrations"
mkdir -p "$dir"

last="$(ls -1 "$dir"/*.sql 2>/dev/null | sort | tail -1 || true)"
num=1
if [[ -n "$last" ]]; then
	num=$(( 10#$(basename "$last" | cut -d_ -f1) + 1 ))
fi

file="$(printf '%s/%04d_%s.sql' "$dir" "$num" "$name")"
if [[ -e "$file" ]]; then
	echo "migration already exists: $file" >&2
	exit 1
fi

cat >"$file" <<EOF
-- +goose Up

-- +goose Down
EOF

echo "created $file"