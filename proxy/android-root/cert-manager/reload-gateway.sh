#!/system/bin/sh
# Fixed deployment layout. No parameters or user-provided commands.
set -eu
BASE=/data/local/diplay-https
[ "$(id -u)" = 0 ] || exit 1
export XDG_DATA_HOME="$BASE/state" XDG_CONFIG_HOME="$BASE/config"
"$BASE/caddy" validate --config "$BASE/Caddyfile" --adapter caddyfile >/dev/null 2>&1
if [ -f "$BASE/caddy.pid" ] && [ ! -L "$BASE/caddy.pid" ]; then
    PID=$(cat "$BASE/caddy.pid")
    case "$PID" in ''|*[!0-9]*) exit 1 ;; esac
    if [ -e "/proc/$PID" ]; then
        [ "$(readlink "/proc/$PID/exe")" = "$BASE/caddy" ] || exit 1
        tr '\000' '\n' < "/proc/$PID/cmdline" | grep -Fx "$BASE/caddy" >/dev/null || exit 1
        kill "$PID"
        COUNT=0
        while [ -e "/proc/$PID" ] && [ "$COUNT" -lt 15 ]; do
            sleep 1
            COUNT=$((COUNT + 1))
        done
        [ ! -e "/proc/$PID" ] || exit 1
    fi
    rm -f "$BASE/caddy.pid"
fi
sh "$BASE/start.sh"