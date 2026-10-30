#!/system/bin/sh
# One persistent entry point. The Go process holds an exclusive instance lock.
BASE=/data/local/diplay-goal-keeper
umask 077
(
    while true; do
        "$BASE/goal-keeper" -config "$BASE/config.json" </dev/null >/dev/null 2>&1
        sleep 30
    done
) </dev/null >/dev/null 2>&1 &
exit 0