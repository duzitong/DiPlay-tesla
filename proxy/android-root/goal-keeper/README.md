# Android root goal keeper

Persistent reconciliation for the V50 Tesla browser gateway; not a fixed sequence
of one-shot boot commands. Every ten seconds it observes Android boot, hotspot
address, DiPlay's HTTP service, certificate-validated HTTPS and its own DNAT rules.
It starts missing processes with backoff and restores missing NAT rules. It does
not force-stop DiPlay, reconnect CarPlay, toggle the hotspot or renew certificates.
The independent certificate manager remains installed separately.

## Actual browser port

DiPlay can select 8080 when it starts before the ZTE web service, or 8081 when
8080 is already occupied. The keeper matches listening socket inodes against
DiPlay process file descriptors, rather than trusting an arbitrary HTTP listener.
Ports 8080–8085 are recognized. It synchronizes the existing Caddyfile's loopback
upstream, Host, Origin and CSP replacement to the discovered port. The initial
Caddyfile is backed up as `Caddyfile.before-port-sync`. If the port changes while
Caddy is running, only the verified gateway process is terminated and restarted;
this can briefly interrupt the browser, but does not stop the CarPlay session.

## Deployment

Build a static Linux ARM64 binary named `goal-keeper`. Stage that binary,
`config.json`, `boot-service.sh` and `install.sh` in a device temporary directory.
Run `sh install.sh STAGING_DIRECTORY` as Magisk root, then reboot.
Use the existing domain/public IPv4, hotspot address and gateway paths in the
configuration: the example IP is not a generally usable public service address.

The installer saves previous binaries/configuration and boot scripts in a dated
directory under `/data/local/diplay-goal-keeper`. It replaces the old
`diplay-https.sh` and `diplay-tesla-nat.sh` boot entries with
`/data/adb/service.d/diplay-goal-keeper.sh`. It does not kill their already-running
loops; reboot is required to complete the change of supervisors.

## Device diagnosis

Read `/data/local/diplay-goal-keeper/status.json` as root. `local_ready` and
`nat_ready` should both be true, with an empty `last_error` and a current timestamp.
Transitions are recorded in bounded `keeper.log` files, without response bodies,
tokens or TLS keys. Readiness is local verification, **not** proof that a Tesla
browser can reach the gateway or negotiate WebRTC.

The keeper waits indefinitely for the hotspot but does not turn it on itself.
It respects a force-stopped app. It does not repair an already-running hung app
or invalid/expired certificates by restarting them repeatedly.

For rollback, remove the new boot entry, restore the previous boot scripts from
the selected dated backup, and reboot. Restore the original Caddyfile from
`Caddyfile.before-port-sync` if reverting the discovered upstream port as well.
Never delete or print `privkey.pem` or the certificate manager credentials.