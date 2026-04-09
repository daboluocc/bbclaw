# Voiceprint Unlock

## Goal

Implement voiceprint unlock only. Other unlock approaches remain TODO.

## Scope In This Repo

- Firmware boots into `LOCKED` for `cloud_saas`
- Locked PTT captures PCM locally and sends `voice.verify`
- Firmware waits for `voice.verify.result` and transitions to `UNLOCKED` on `match=true`
- LVGL adds a dedicated lock screen
- Protocol documentation defines the new WebSocket messages

## Constraints

- The cloud service source described in the issue (`references/cloud/...`) is not present in this repository or upstream `main`
- Because of that, this change can only implement the firmware-side flow and document the expected cloud contract

## Firmware Notes

- Verification audio is buffered in PSRAM as PCM16 mono
- Capture is capped by `BBCLAW_VOICE_VERIFY_MAX_MS`
- Existing pairing / auth / cloud readiness screens remain visible until cloud audio is actually ready, then the lock screen takes over

## External TODO

- Add cloud stub endpoint `POST /api/voiceprint/verify`
- Translate `voice.verify` WebSocket requests into verifier calls
- Replace stub logic with 火山引擎 Speaker Verification API
