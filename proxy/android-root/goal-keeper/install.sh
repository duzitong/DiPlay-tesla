#!/system/bin/sh
# Run as root with the staging directory as the sole argument, then reboot.
set -eu
STAGE="${1:?staging directory required}"
BASE=/data/local/diplay-goal-keeper
[ "$(id -u)" = 0 ] || exit 1
umask 077
mkdir -p "$BASE"
chmod 700 "$BASE"
cp "$STAGE/goal-keeper" "$BASE/goal-keeper.new"
chmod 700 "$BASE/goal-keeper.new"
"$BASE/goal-keeper.new" -config "$STAGE/config.json" -check
BACKUP="$BASE/backup-$(date +%Y%m%d-%H%M%S)"
mkdir "$BACKUP"
for name in goal-keeper config.json; do
    if [ -f "$BASE/$name" ]; then cp -p "$BASE/$name" "$BACKUP/$name"; fi
done
cp "$STAGE/config.json" "$BASE/config.json"
chmod 600 "$BASE/config.json"
mv "$BASE/goal-keeper.new" "$BASE/goal-keeper"
for name in diplay-https.sh diplay-tesla-nat.sh diplay-goal-keeper.sh; do
    if [ -f "/data/adb/service.d/$name" ]; then
        cp -p "/data/adb/service.d/$name" "$BACKUP/$name"
    fi
done
cp "$STAGE/boot-service.sh" /data/adb/service.d/diplay-goal-keeper.sh.new
chmod 700 /data/adb/service.d/diplay-goal-keeper.sh.new
mv /data/adb/service.d/diplay-goal-keeper.sh.new /data/adb/service.d/diplay-goal-keeper.sh
rm -f /data/adb/service.d/diplay-https.sh /data/adb/service.d/diplay-tesla-nat.sh
echo "Installed. Old boot scripts saved in $BACKUP. Reboot to switch supervisors."