# Plan: H.264 passthrough + WebCodecs canvas

Status: proposal only; nothing implemented. Branch: `plan/webcodecs-canvas-renderer`, based on
`fix/browser-playback-recovery-diagnostics` at `384f36c`.

## Scope: replace JPEG conversion, not the CarPlay stack

Use the H.264 input already available at `rtc/BrowserVideoOutput.onVideoConfig/onVideoFrame`,
before `JpegVideoSink`. No extraction from JPEG or WebRTC is needed.

```text
Current JPEG: H.264 -> Android decode -> YUV -> JPEG -> HTTP snapshots -> image decode -> canvas
New path:     H.264 -> video WebSocket -> browser VideoDecoder -> VideoFrame -> canvas
```

Keep CarPlay reception, authentication, touch, PCM audio, layout and the existing HTTPS entry.
WebRTC remains the default; retain JPEG unchanged as a manual fallback, not an optimisation project.
This is one alternative video path, not a DashPlay port or a new player/transport framework.

## 1. Forward the encoded frames

- Add an H.264 output selected by `BrowserVideoOutput`. Suspend JPEG production while selected:
  no Android decode, YUV conversion, JPEG encoding or `LatestFrame` demand lease.
- Reuse `RtcAccessUnit`, `RtcVideoConfig`, `RtcFrameQueue` and the existing rate-limited recovery
  callback for Annex B/SPS/PPS, keyframes, owned buffers and reference-chain recovery.
- Use one dedicated `/api/video` WebSocket, not the PCM/signaling writer. Bind it to the existing
  authenticated `BrowserMediaHub` owner, not a second viewer; revoke it on owner detach.
  Preserve origin/token checks, bounded messages and connection limits.
- Send config per generation, then complete access units with generation, sequence, relative
  timestamp (microseconds) and key flag. Include SPS/PPS on keyframes.
- Use cumulative progress ACKs with fixed in-flight frame/byte limits, not bandwidth estimation.
  Time out only pending work. Close a stalled video link to discard its TCP backlog, then restore
  config and a keyframe; bound retries and report failure. Do not detach the audio owner.

## 2. Decode and paint in the existing page

- Add main-thread `webcodecs-renderer.js`: draw `VideoDecoder` output to the existing canvas and
  close each `VideoFrame` in `finally`. Release resources on reset, error and teardown.
- Check `isSecureContext`, `VideoDecoder` and `isConfigSupported` for the actual H.264 config.
  Use trusted HTTPS/WSS; plain LAN HTTP is insufficient. Do not gate other modes on this check.
- Use Annex B without `description`; derive `avc1.PPCCLL` from SPS bytes 1..3 using the same
  calculation as `RtcVideoConfig`, not SDP parsing. Reject HEVC/unsupported configs explicitly;
  do not change iPhone settings automatically.
- Bound decode backlog. A discarded prediction chain requires decoder reset and a fresh keyframe,
  not JPEG-style "latest frame wins". ACK consumed frames; ignore stale generations/callbacks.
- Keep the last image during recovery; resize only on dimension changes. Reuse existing diagnostics
  for decode/paint counts, backlog and errors. No new video worker, OffscreenCanvas or dashboard.

## 3. Connect the existing mode and lifecycle plumbing

- Add opt-in `webcodecs` to **video transport** alongside `auto` and `jpeg`, not WebRTC's Native/Canvas
  renderer selector. Preserve existing preferences; follow existing experimental-label/i18n conventions.
- Wire `app.js`, `media.js`, the hello, hub/output and status summaries; register the new asset/video
  route through the existing HTTPS entry. WebCodecs mode must start neither RTC nor JPEG polling.
- Reuse mode-switch close/open, including its existing audio reset, and hidden-page teardown.
  Invalidate generations on switch/disconnect/config change. No seamless-switch framework or
  forced iPhone reconnect. Surface unsupported decoding/exhausted recovery; fallback is manual.

## Acceptance

- Browser JVM/JS tests cover authentication/ownership, config/keyframe ordering, queue/in-flight
  bounds, recovery/stale generations and frame cleanup. Existing WebRTC/JPEG tests still pass.
- Real-browser smoke check: visibly changing picture (not just FPS), working audio/touch, bounded
  resources and no Android JPEG pipeline running. Slow video must not reset the audio owner.
  Record actual compatibility; WebCodecs does not imply exemption from vehicle/browser policies.

## Explicitly out of scope

Standalone probe pages/matrices, adaptive bitrate/FPS, HEVC, WebTransport, JPEG optimisation,
unrelated RTC fixes and audio/Bluetooth redesign. No forced playback or motion-state workarounds.
DashPlay/PlayPort are references, not dependencies; retain notices if adapting implementation code.
