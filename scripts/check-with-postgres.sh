#!/usr/bin/env bash
set -euo pipefail
pg_bin="${STUHELPER_TEST_PG_BIN:-/usr/lib/postgresql/18/bin}"
if [[ -d "$PWD/.tools/postgres/root" ]]; then
  pg_bin="$PWD/.tools/postgres/root/usr/lib/postgresql/18/bin"
  export LD_LIBRARY_PATH="$PWD/.tools/postgres/root/usr/lib/x86_64-linux-gnu${LD_LIBRARY_PATH:+:$LD_LIBRARY_PATH}"
fi
mkdir -p .tools
# Short Unix socket path, unique to this task and removed on all exits.
task_dir="$(mktemp -d /tmp/stuhelper-groups-check.XXXXXX)"
cleanup() {
  "$pg_bin/pg_ctl" -D "$task_dir/data" -m immediate stop >/dev/null 2>&1 || true
  case "$task_dir" in /tmp/stuhelper-groups-check.*) rm -rf -- "$task_dir" ;; esac
}
trap cleanup EXIT INT TERM
mkdir "$task_dir/socket"
init_args=()
if [[ -d "$PWD/.tools/postgres/root" ]]; then init_args=(-L "$PWD/.tools/postgres/root/usr/share/postgresql/18"); fi
"$pg_bin/initdb" "${init_args[@]}" -D "$task_dir/data" --auth-local=trust --auth-host=reject --no-locale --encoding=UTF8 >/dev/null
"$pg_bin/pg_ctl" -D "$task_dir/data" -l "$task_dir/server.log" -o "-h '' -k $task_dir/socket -p 55439" -w start >/dev/null
export STUHELPER_GROUPS_TEST_DSN="host=$task_dir/socket port=55439 user=$(id -un) dbname=postgres sslmode=disable"
"$pg_bin/postgres" --version
if [[ "$#" -gt 0 ]]; then "$@"; else make check; fi
