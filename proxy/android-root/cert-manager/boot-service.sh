#!/system/bin/sh
BASE=/data/local/diplay-cert-manager
umask 077
nohup sh "$BASE/supervise.sh" </dev/null >/dev/null 2>&1 &
exit 0