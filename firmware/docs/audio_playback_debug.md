# Audio Playback Debug

Golden sample: `firmware/assets/svg/bbclaw.wav`

Expected duration for current boot WAV:

- bytes: `41410`
- format: `PCM16 mono 16000 Hz`
- expected_ms: `1294`

## Results

| Build / Context | TX config | expected_ms | actual_ms | ratio_x100 | Audible | Timeout | occupied/not usable | Notes |
| --- | --- | ---: | ---: | ---: | --- | --- | --- | --- |
| historical, full-duplex shared TX/RX | stereo32 `[v,v]` | 1294 | 2407-2428 | 185-187 | yes | yes | no | Old path. Correctly produced sound, but playback was about 1.86x slow. |
| historical, simplex coexist attempt | stereo16 `[v,v]` | 1294 | ~2430 | ~187 | no | unknown | yes | Triggered `i2s controller occupied` and `GPIO not usable`; invalid experiment shape. |
| historical, mode-switched simplex | stereo16 `[v,v]` | 1294 | ~2430 | ~187 | no | unknown | no | Conflict removed, but playback still slow and effectively silent. |
| current default (`BBCLAW_AUDIO_TX_EXPERIMENT=1`) | stereo32 `[v,v]` | 1294 | 2417 | 186 | yes | yes (81) | no | Experiment A failed. Removing shared full-duplex did not change the slow-playback ratio; root cause is in TX stereo32 semantics/config itself. |
| `BBCLAW_AUDIO_TX_EXPERIMENT=2` | stereo16 `[v,v]` | 1294 | 2418 | 186 | no | yes (81) | no | Experiment B also failed with the exact same duration and timeout count as A. This ruled out stereo32 vs stereo16 as the primary cause. |
| current default (`BBCLAW_AUDIO_TX_EXPERIMENT=1`) | stereo32 `[v,v]` + fixed I/O timeout units | 1294 | 1293 | 99 | yes | no (0) | no | Resolved. Root cause was timeout unit misuse in new I2S API. Boot WAV restored to expected duration; TTS playback also returned to normal timing. |

## Notes

- Do not use `BBCLAW_AUDIO_TX_RATE_COMP_*` as a fix. Keep `1/1`; it is only a diagnostic escape hatch.
- `i2s_channel_read/write()` in the new driver take timeout in milliseconds, not RTOS ticks. Passing `200 / portTICK_PERIOD_MS` caused repeated premature timeouts and dominated playback latency.
- Required boot logs for each run:
  - `i2s prep=...`
  - `playback start ...`
  - `boot wav play ...`
  - `play_pcm ... elapsed_ms=... timeouts=...`
  - `boot wav done expected_ms=... actual_ms=... ratio_x100=...`
  - `playback stop`
- Experiments A and B both failed before the timeout fix; no further TX format experiment was needed after fixing timeout units because Experiment A then passed at `ratio_x100=99` with `timeouts=0`.
