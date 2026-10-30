#!/system/bin/sh
# One-time installation. Existing gateway and TLS files are NOT modified.
set -eu
STAGE=/data/local/tmp/diplay-cert-manager-stage
BASE=/data/local/diplay-cert-manager
SERVICE=/data/adb/service.d/diplay-cert-manager.sh
[ "$(id -u)" = 0 ] || { echo 'Run this installer as root yourself.'; exit 1; }
umask 077
[ ! -e "$BASE" ] && [ ! -L "$BASE" ] || { echo 'Existing installation: refusing overwrite.'; exit 1; }
[ ! -e "$SERVICE" ] && [ ! -L "$SERVICE" ] || exit 1
for F in cert-manager config.json reload-gateway.sh supervise.sh boot-service.sh; do
    [ -f "$STAGE/$F" ] && [ ! -L "$STAGE/$F" ] || exit 1
done
[ -d /data/adb/service.d ] && [ ! -L /data/adb/service.d ] || exit 1
[ -d /data/local/diplay-https ] && [ ! -L /data/local/diplay-https ] || exit 1
mkdir -m 700 "$BASE"
for F in cert-manager config.json reload-gateway.sh supervise.sh; do
    cp "$STAGE/$F" "$BASE/$F"
    chown 0:0 "$BASE/$F"
done
chmod 600 "$BASE/config.json"
chmod 700 "$BASE/cert-manager" "$BASE/reload-gateway.sh" "$BASE/supervise.sh"
export SSL_CERT_DIR=/apex/com.android.conscrypt/cacerts:/system/etc/security/cacerts
"$BASE/cert-manager" -config "$BASE/config.json" -check
"$BASE/cert-manager" -config "$BASE/config.json" -init-password
cp "$STAGE/boot-service.sh" "$SERVICE"
chown 0:0 "$SERVICE"
chmod 700 "$SERVICE"
sh "$SERVICE"
echo 'Manager installed. Existing HTTPS gateway unchanged.'
echo 'Admin password is root-only: /data/local/diplay-cert-manager/state/admin-password'
echo 'Read/save it privately in your terminal; never paste it into chat.'