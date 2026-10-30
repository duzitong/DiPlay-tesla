#!/system/bin/sh
# Independent manager only. Does not stop/start DiPlay or the existing gateway.
set -eu
BASE=/data/local/diplay-cert-manager
[ "$(id -u)" = 0 ] || exit 1
umask 077
if [ -f "$BASE/supervisor.pid" ]; then
    OLD=$(cat "$BASE/supervisor.pid")
    case "$OLD" in
        ''|*[!0-9]*) ;;
        *) if [ -r "/proc/$OLD/cmdline" ] && tr '\000' '\n' < "/proc/$OLD/cmdline" | grep -Fx "$BASE/supervise.sh" >/dev/null; then exit 0; fi ;;
    esac
fi
echo $$ > "$BASE/supervisor.pid"
trap 'rm -f "$BASE/supervisor.pid"' EXIT
export SSL_CERT_DIR=/apex/com.android.conscrypt/cacerts:/system/etc/security/cacerts
while :; do
    if ip -4 addr show dev br0 2>/dev/null | grep -q 'inet 192[.]168[.]0[.]1/'; then
        # Bound log growth across restarts. No request bodies/credentials logged.
        if [ -f "$BASE/manager.log" ] && [ "$(wc -c < "$BASE/manager.log")" -gt 262144 ]; then
            mv -f "$BASE/manager.log" "$BASE/manager.log.previous"
        fi
        "$BASE/cert-manager" -config "$BASE/config.json" >> "$BASE/manager.log" 2>&1 || :
    fi
    sleep 30
done