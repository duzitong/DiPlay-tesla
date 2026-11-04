# Optional DiPlay proxy

This Node.js 22+ process is deployed independently of the Android CarPlay host. No Android
module imports it or requires it. Direct HTTP access uses the actual address shown in
Android Connection → Tesla browser, where the vehicle firmware permits it. There are no
npm dependencies. For APK installation, iPhone connection and browser pairing, start with
the [user guide](https://github.com/WangZhuo2015/DiPlay-tesla/blob/main/docs/TESLA-QUICKSTART.md)
or [中文使用指南](https://github.com/WangZhuo2015/DiPlay-tesla/blob/main/docs/TESLA-QUICKSTART.zh-CN.md).

For HTTPS hosted on a rooted Android hotspot device, see the independent
[Android root gateway](android-root/README.md). Its public configuration template contains
only placeholders; real domains, routing addresses and certificate configuration stay local.

## Browser gateway

```powershell
$env:DIPLAY_UPSTREAM = 'http://192.168.1.20:8080'
$env:DIPLAY_BIND = '0.0.0.0'
$env:DIPLAY_PORT = '8090'
node proxy/server.mjs
```

Open `http://GATEWAY_IP:8090` and use the pairing code from the Android host. This is a fixed
reverse proxy for the browser interface. It preserves pairing authentication, changes a
same-origin browser request to the upstream origin, and reports 502 if the host is unavailable.
An HTTPS terminator can be put in front of this service; send the original Host header.

The gateway forwards `/api/media` WebSocket signaling and optional PCM audio, preserving
the first-message pairing check. HTTPS uses WSS on that same origin. WebRTC video travels
directly between the browser and Android over UDP/DTLS-SRTP; both need direct network
reachability. This process is not a TURN server or UDP relay. If video cannot establish
that direct path, clients before 0.1.14 may fall back to gateway-forwarded JPEG. From
0.1.14, JPEG is an explicit manual H.264 diagnostic mode;
WebRTC negotiation/recovery never starts JPEG conversion automatically. The gateway
does not transcode HEVC, and enabling HEVC does not add a UDP relay.

**H.264 / WebCodecs (experimental)** is an opt-in video transport in browser Display
settings, separate from WebRTC's Native/Canvas presentation setting. It forwards the
original H.264 access units over a dedicated `/api/video` WSS connection and decodes
them directly to canvas. Android JPEG decoding/conversion stays suspended. This
requires trusted HTTPS and browser support for the actual H.264 configuration; HEVC
is rejected without changing the iPhone settings. WebRTC remains the default and JPEG
remains manual. Unsupported decoding or exhausted video recovery requires a manual
transport switch; it does not silently fall back or reconnect CarPlay.

The video socket uses the existing pairing token plus the current media owner's
session lease. Configuration generations, keyframes and cumulative consumed-frame
ACKs bound both queues and in-flight video to six frames / 2 MiB each. Expiry or
overflow of the 50 ms **unsent** queue drops its dependent frames and requests a
fresh keyframe on the same ordered video link, retaining the last picture. It does
not spend a transport retry. Invalid input, send failure or a three-second
pending-video/keyframe timeout closes only the video link, then requests
configuration and a fresh keyframe (at most three recovery retries).
Idle sources do not trigger a decode timeout. `/api/media` PCM/signaling remains
independent. WebCodecs does not bypass vehicle/browser restrictions; desktop
compatibility must not be treated as proof of compatibility in a Tesla.

WebCodecs diagnostics distinguish decoder outputs from successful canvas draw
submissions. A consumed-frame ACK confirms the draw submission, not compositor
presentation or physical display scanout. These counters do not measure
glass-to-glass latency.

## Split navigation and iPhone Bluetooth audio

With the all-Bluetooth switch **off**, the Tesla APK reserves CarPlay's main-audio
resource for the native source while keeping the independent alternate navigation
channel available. Select the car's Bluetooth output on the iPhone for music.
Navigation prompts then use the browser and its guidance gain; DiPlay does not
relay the Bluetooth audio or choose a Bluetooth device on the phone.

The complete nine-format/nine-latency declaration is intentional: removing music
formats also removed Amap's navigation audio on the tested iPhone. Main-audio
resource constraints implement the split instead, with receiver-side rejection of
main-audio SETUP (100/102/103) as a guard, not a browser music-mute workaround.
Only alternate-audio SETUP (101) is accepted in this profile. Buffered music is
not enabled even if its shared preference was previously saved.

Desktop testing with music audible in iPhone-connected AirPods and navigation in
the browser confirmed the split. Tesla Bluetooth selection, firmware-specific
audio-focus behavior, calls and Siri still need their own in-car validation.

The Tesla APK's **Advanced → CarPlay audio over the car's Bluetooth (experimental)**
opt-in disables all CarPlay audio offers, including navigation. Music, navigation,
Siri and calls use the **iPhone's direct connection to the car**, not a Bluetooth
relay from DiPlay. Pair the iPhone with the car and select the car's Bluetooth
output on the phone; otherwise audio may play on the iPhone. Changing this switch
reconnects CarPlay. The default remains off; other APK targets retain their
existing audio policy.
Disabled audio also declines audio SETUP requests without affecting video SETUP.
The browser shows the Bluetooth policy, disables its audio controls and closes any
existing AudioContext without changing saved browser volumes or playback preferences.
It does not create an audio output in this mode; video and touch remain available.
Closing the browser does not control the independent iPhone Bluetooth audio path.
Actual Bluetooth output and background navigation still require in-car verification.

Use **Menu → Show virtual keys** to test Previous, Play, Pause and Next before
connecting car controls. Play and Pause send separate CarPlay media HID commands;
the legacy `play` API action remains a toggle. “Submitted” confirms the receiver
accepted the request, not that the iPhone executed it. Receiver logs record whether
the press and release were written to its event channel. Test with both the music
app and navigation visible; these buttons do not adjust Bluetooth navigation volume
or claim the browser's system media session.
The media HID is global, without a `displayUUID`; binding it to the CarPlay display
prevents these playback commands from reaching the player in the Bluetooth split.
During CarPlay, the app-sized lower-left **Next** shortcut above the app-menu button sends
the same media command without opening the virtual keys or changing the iPhone's
foreground app. It follows the displayed picture when letterboxed and hides
when disconnected or when the full virtual-key row is open. Extra spacing separates
it from the app-menu button to reduce accidental presses.
Large-font full, compact/high-density and Arabic RTL shortcut screenshots are in
`docs/testing/tesla/next-track-shortcut-20261104/`.
Whitelisted media/arrow keydown events are logged without intercepting them, so
an in-car test can establish which steering-wheel events actually reach the page.

The virtual **Guidance − / +** buttons change only the browser's navigation gain
in 1 dB steps, sharing the slider and its saved dB value.
They send no phone volume keys and are disabled in all-Bluetooth mode. Navigation
must actually arrive on CarPlay's guidance stream; music sent directly to the car
over Bluetooth bypasses these controls.

When all-Bluetooth mode is off, **Guidance gain (dB)** defaults to **0 dB** and
ranges from −60 dB (mute) to **+24 dB**. It uses a new browser preference
(`diplay-navigation-gain-db`); old percentage boosts are not automatically
restored. Start at +6 dB and raise it gradually. At 0 dB the original waveform
is unchanged below the protection ceiling, apart from 5 ms of lookahead delay.
Positive gain enables navigation-only soft-knee compression (−12 dB threshold,
3:1 ratio, 3 ms attack / 40 ms release), followed by a stereo-linked lookahead
limiter with a **−1 dBFS sample-peak ceiling** and 20 ms release.
Its gain ramps across the lookahead window before a transient is played, rather
than hard-clamping individual samples at the output.
Compressor gain reduction is capped at the requested boost so enabling a small
positive setting does not attenuate the original signal before peak protection.
Requested gain is not an equal increase in perceived loudness. This is not a
true-peak guarantee, nor a repair for distorted source audio or speaker overload.
If the protection worklet cannot load, positive gain is disabled and the browser
shows a warning; attenuation and mute still work. A processor failure releases
audio rather than bypassing protection.

Music gain remains limited to 0–100% on receivers that play browser music; that control
does not adjust music sent directly from the iPhone in the split profile.
Adjustments apply immediately, persist in this browser,
and affect neither music gain nor the car's Bluetooth volume or existing ducking.
The separate host navigation attenuation
in Advanced audio remains shared with Android and still applies before this gain.
Large-font full, compact/high-density and Arabic RTL screenshots are in
`docs/testing/tesla/navigation-db-20261104/`.

## HTTPS browser gateway (0.1.1+)

The Android APK serves HTTP, starting at port 8080 and trying later ports when firmware
already occupies it. Use the actual port shown in Connection → Tesla browser as the
upstream in the examples below. TLS lives in this independent process; changing that
Android URL to `https://` does not enable encryption. All domain, certificate/key and
network settings are supplied by the user; this download contains templates only.

Some Tesla firmware restricts RFC1918 private-IP access (`10.0.0.0/8`,
`172.16.0.0/12`, `192.168.0.0/16`). A working phone/desktop request does not establish
vehicle access, and a domain or TLS alone may not resolve it. Configure an entry address
the car can reach and its firmware permits, then verify in the car. See the
[private-IP deployment notes](https://github.com/WangZhuo2015/DiPlay-tesla/blob/main/docs/TESLA-QUICKSTART.md#tesla-private-ip-access-restrictions)
for the developer report and configuration boundaries.

Prepare a domain and a certificate chain/key for it that the vehicle browser trusts.
Keep the key outside Git. On the gateway device, set:

```powershell
$env:DIPLAY_UPSTREAM = 'http://192.168.1.20:8080' # Android host reachable from this device
$env:DIPLAY_BIND = '0.0.0.0'
$env:DIPLAY_PORT = '8443'
$env:DIPLAY_TLS_CERT = 'C:\certs\fullchain.pem'
$env:DIPLAY_TLS_KEY = 'C:\certs\privkey.pem'
node proxy/server.mjs
```

Then open **`https://YOUR_DOMAIN:8443`**, using the Android pairing code. Resolve that domain
to a gateway address the vehicle can reach and its firmware permits. The certificate must
cover the domain, be valid and include its intermediate chain. Do not expect a self-signed
certificate or a domain certificate opened by IP address to be trusted by the Tesla browser.
Port 443 can be selected with `DIPLAY_PORT`; it removes `:8443` from the URL and may require
OS permissions or router forwarding. The gateway machine must be able to reach Android's
actual HTTP port.

TLS defaults to port 8443 and a minimum of TLS 1.2. Supplying only one credential, unreadable
files or an invalid key/certificate stops startup; it never silently falls back to HTTP.
An encrypted key can use `DIPLAY_TLS_KEY_PASSPHRASE`. Restart the process after renewing the
certificate. No certificate acquisition, DNS, internet sharing, NAT or vehicle restrictions
are managed by this gateway. HTTPS alone does not resolve firmware-specific private-network
restrictions or make the host reachable over the internet.

[Let's Encrypt DNS-01](https://letsencrypt.org/docs/challenge-types/#dns-01-challenge) can
validate a domain without exposing this gateway to the internet; certificate issuance and
vehicle reachability are separate tasks. HTTPS transport certificates are separate from
the MFi identity required for the iPhone's CarPlay authentication.

## Explicit web proxy

An authenticated HTTP/HTTPS forward proxy can be enabled separately:

```powershell
$env:DIPLAY_FORWARD_PORT = '8888'
$env:DIPLAY_PROXY_TOKEN = 'choose-a-random-secret-at-least-24-characters'
node proxy/server.mjs
```

Clients must explicitly select this proxy and send `Proxy-Authorization: Bearer SECRET`.
Only destination ports 80 and 443 are accepted. It binds to loopback unless `DIPLAY_BIND`
is set. Do not expose it beyond your trusted network.

**This service does not create a hotspot, install a VPN, provide NAT/DNS, or give Tesla internet
access automatically.** A Tesla browser has no proxy setting managed by DiPlay. Use the
Android device's internet-sharing hotspot or an independently configured router for that.
Stopping either proxy has no effect on the iPhone/Android CarPlay session; direct browser
access to the host remains available.

Run `node --test proxy/test/*.test.mjs` from the repository root.
The HTTPS tests generate ephemeral certificates using OpenSSL. On Windows, set
`DIPLAY_TEST_OPENSSL` to your OpenSSL executable if it is not on PATH. Test keys are removed
after the run and are not shipped or used for a vehicle connection.
