#!/bin/sh
set -eu

umask 077
repo_dir=$(CDPATH= cd -- "$(dirname -- "$0")/.." && pwd -P)
cd -- "$repo_dir"
mkdir -p "$repo_dir/.local/state"

export AUTODOCKER_ROOT="$repo_dir/.local/state"
export BIND_HOST=127.0.0.1
export PORT=18743
export AUTODOCKER_GENERATOR_ONLY=0
export AUTODOCKER_ACCOUNT_OWNER=admin
export AUTODOCKER_LOCAL_BOOTSTRAP=1
export AUTODOCKER_LOCAL_HTTP=1
export AUTH_BYPASS=0
export DEMO=0
export AUTODOCKER_OPEN_BROWSER=1

AUTODOCKER_BOOTSTRAP_TOKEN=$(od -An -N32 -tx1 /dev/urandom | tr -d ' \n')
if [ "${#AUTODOCKER_BOOTSTRAP_TOKEN}" -ne 64 ]; then
  printf '%s\n' 'Impossible de préparer le premier lancement sécurisé.' >&2
  exit 1
fi
export AUTODOCKER_BOOTSTRAP_TOKEN

exec go run .
