# Plan: WebCodecs → canvas renderer for the Tesla browser

Status: proposal, nothing implemented. Branch: `plan/webcodecs-canvas-renderer`, cut from
`fix/browser-playback-recovery-diagnostics`.

## Problem

The Tesla browser pauses `<video>` playback when the car is not in P (observed: video freezes, the app
asks the iPhone for a new level, the stream resets, the new page freezes again). The existing Canvas
option (`rtc-renderer.js`) only repaints frames from the same `<video>`, so it stalls with it. The JPEG
path draws to a canvas without a media element but lags and uses much more bandwidth.

Use case: a passenger-facing CarPlay/navigation view. This plan does not try to keep a `<video>`
element playing against the car's pause (no `play()` loops, no resume button, no gear/motion spoofing).
It adds a renderer that does not use a media element.

## Unverified assumptions (test before building)

1. The Tesla browser exposes WebCodecs `VideoDecoder` with H.264 support.
2. Tesla's pause targets media elements, not canvas painting or WebCodecs decode. Unknown; step 1 tests it.
3. Annex B H.264 without a `description` decodes in that Chromium version.

If step 1 fails, stop and optimise the JPEG path instead (see "Fallback").

## Steps

### 1. Capability probe (no Kotlin changes)
New `browser/src/main/assets/tesla/webcodecs-probe.html`, loaded from the existing server. On-screen overlay reports:
- `navigator.userAgent`, `isSecureContext`, `typeof VideoDecoder`, `OffscreenCanvas`, `createImageBitmap`.
- `VideoDecoder.isConfigSupported` for avc1 baseline/main/high, hardware and software.
- Decode of a bundled short Annex B clip: decode ms/frame, frames painted, `VideoFrame.close()` leak check.
- Repeat while parked and with a passenger running it in D. Record: does decode keep running, does the canvas
  keep updating, does a control `<video>` pause.
Exit criterion: decode + canvas keep running in D on a real car.

### 2. Transport (Kotlin, `browser/src/main/java/com/diplay/browser/`)
`BrowserMediaSocket` is built for audio and signaling (32-deep writer queue, 2048-byte inbound cap, comment says
video goes over WebRTC). Do not put video on it: audio would wait behind keyframes.
- Add a separate video WebSocket (or a priority lane) owned by `BrowserMediaHub`. Check how the hub's
  single-owner `media-busy` rule applies (not yet read: `BrowserMediaHub.kt`).
- Binary frame: small header (key flag, timestamp, sequence) + Annex B payload.
- Page sends an ack per frame/batch; sender drops frames while unacked bytes exceed a budget (about 300 ms at
  the measured rate), then resumes at a keyframe. Design reference: DashPlay `playport-viewer-lag-gate.patch`
  (GPL-3.0; reimplement, do not copy).

### 3. Sink (Kotlin)
New `WebCodecsVideoSink` beside `JpegVideoSink`, fed from `rtc/BrowserVideoOutput.onVideoFrame`.
- Reuse `RtcAccessUnit.inspect` (key detection) and `copyAnnexB(prefix)` (SPS/PPS on keyframes).
- Reuse `RtcFrameQueue` (6 frames / 2 MB / 50 ms; drops the dependent chain, sets `needsKey`) and `requestKey()`
  (rate limited to 500 ms) for recovery.
- Extend `BrowserVideoOutput.transport` with a `"webcodecs"` value; keep JPEG as the fallback.
- Unit tests alongside the existing queue/output tests.

### 4. Page renderer (`browser/src/main/assets/tesla/webcodecs-renderer.js`)
- `VideoDecoder` configured from the SPS (codec string derived from `RtcVideoConfig`, to verify), Annex B, no `description`.
- Paint only the newest decoded frame on `requestAnimationFrame`; always `frame.close()`.
- Never clear or resize the canvas for same-size frames (see DashPlay `playport-viewer-steady-picture.patch`).
- Backlog rule: if >= 8 frames are queued in the decoder and the oldest is > 200 ms old, skip to the next keyframe and
  request one (DashPlay `playport-decode-backlog.patch`).
- Stats for the existing diagnostics export: decode ms, paint ms, queue depth, skips.

### 5. Mode plumbing
- New renderer option next to Native/Canvas in `index.html`/`app.js` (with translations).
- Automatic fall back to JPEG on `VideoDecoder` error or unsupported config.
- Treat a system pause as a state, not a failure: no reconnect loop (see "Independent fixes").

## Independent fixes (worth doing regardless)

Canvas-mode blinking, from reading `rtc-renderer.js` (hypotheses, confirm with an exported log):
- `showNative()` removes `rtcOffscreen` and hides the canvas on every `restart()`, flashing the native video.
- JPEG `resetPicture()`/transport switches clear the shared canvas.
- Canvas resize clears the backing store (black with `alpha:false`).
- `media.js` health checks treat a paused video as failure, which may drive the reset loop.
Fix: never hide the canvas or show the native video during restart; keep the last frame until a new one is drawn;
do not reconnect on a system pause.

## Fallback if step 1 fails

Optimise the JPEG path: latest-frame-wins with ack pacing, `createImageBitmap` decode, binary transport, dirty-rect
or identical-frame skipping, adaptive quality from RTT.

## Diagnostics to collect on the car (parked, or run by a passenger)

- Exported log after a freeze: count of `pause`, `renderer-restart`, `canvas-clear`, `canvas-resize`; whether
  `paused` returns to `false` between pauses.
- Parked vs D: does Canvas mode blink parked too?
- Glass-to-glass latency (ms counter on the sender, filmed next to the screen) for WebRTC, JPEG, WebCodecs.
- 10-minute soak for `VideoFrame` leaks.

## Licensing

DashPlay and PlayPort are GPL-3.0; WheelPlay is already credited in `rtc-renderer.js`. Use their designs as
reference and add attribution in `docs/THIRD_PARTY_NOTICES.md` if any code is adapted.

## Open questions

- How `RtcVideoConfig` exposes profile/level for the WebCodecs codec string.
- Hub ownership with two sockets (`BrowserMediaHub.kt`).
- Whether the Tesla browser needs a secure context for WebCodecs (the page is already served over HTTPS/token; verify).
