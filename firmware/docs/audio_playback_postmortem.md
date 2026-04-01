# BBClaw Audio Playback Postmortem

## Summary

This issue lived in the `firmware` layer, specifically the local audio playback chain:

- boot welcome WAV playback
- cloud TTS playback
- ESP-IDF I2S TX read/write timeout handling

User-visible symptom:

- the boot welcome audio and TTS both played at about `1.86x` slow speed
- some intermediate experiments also produced "no sound"

Final result:

- boot WAV returned to normal speed
- TTS playback returned to normal speed
- RX capture remained healthy

Confirmed good result from the final log:

- boot WAV: `expected_ms=1294 actual_ms=1293 ratio_x100=99`
- boot WAV TX: `play_pcm stereo32 ... elapsed_ms=1291 timeouts=0`
- TTS TX: `play_pcm stereo32 ... samples=63360 elapsed_ms=3939 timeouts=0`
- capture RX: `capture summary ... timeouts=0`

## Impact

The bug affected the end-to-end voice experience even when cloud services were healthy:

- startup self-test audio sounded obviously slow
- TTS replies also sounded slow
- the symptom initially looked like an I2S slot/frame configuration problem

This made the failure easy to misdiagnose as:

- sample-rate mismatch
- stereo/mono slot mismatch
- full-duplex clock sharing issue
- MAX98357A format incompatibility

## Timeline And Evidence

### 1. Initial confirmed failure

The old shared full-duplex path produced audible sound, but timing was badly wrong:

- boot WAV expected duration: `1294 ms`
- actual duration: about `2407-2428 ms`
- effective slowdown: about `1.86x`

This was later reproduced in the new experiment framework as:

- `play_pcm stereo32 ... elapsed_ms=2416 timeouts=81`
- `boot wav done expected_ms=1294 actual_ms=2417 ratio_x100=186`

### 2. Temporary clock compensation was not a fix

We added a temporary playback rate compensation knob:

- `BBCLAW_AUDIO_TX_RATE_COMP_NUM`
- `BBCLAW_AUDIO_TX_RATE_COMP_DEN`

That changed the symptom, but only by forcing the I2S TX clock:

- `17/10` made playback too fast
- `15/10` also overshot badly

This proved the ratio knob was only a diagnostic escape hatch, not a root fix.

### 3. Full-duplex conflict hypothesis was only partially true

We then moved away from the shared full-duplex TX/RX shape and tried simplex-based playback/capture management.

What this did fix:

- removed `i2s controller 0 has been occupied by i2s_driver`
- removed `GPIO 15 is not usable`
- removed `GPIO 16 is not usable`

What it did not fix:

- playback was still about `1.86x` slow

Conclusion:

- GPIO/controller conflicts were real, but they were not the root cause of the slow-speed symptom.

### 4. TX format experiment matrix did not isolate the problem

We formalized the playback experiments:

- Experiment A: TX-only `stereo32`, app writes `[v,v]`
- Experiment B: TX-only `stereo16`, app writes `[v,v]`
- Experiment C: TX-only `mono16`, explicit left slot

Key result:

- Experiment A failed with `elapsed_ms=2416`, `timeouts=81`, `ratio_x100=186`
- Experiment B failed with essentially the same numbers

This ruled out `stereo32` vs `stereo16` as the primary explanation.

The repeated clue was the timeout count:

- `timeouts=81`

That number was too stable to be incidental.

## Root Cause

The real bug was a timeout unit mismatch in the firmware I2S code.

The new ESP-IDF I2S channel APIs expect timeout values in **milliseconds**:

- `i2s_channel_read(...)`
- `i2s_channel_write(...)`

But the code was still passing:

```c
200 / portTICK_PERIOD_MS
```

That is an RTOS tick conversion pattern, not a millisecond timeout.

On this device configuration, that effectively reduced the intended timeout from `200 ms` to about `20 ms`, which caused repeated premature timeout/retry behavior during playback and capture loops.

That in turn inflated wall-clock playback duration. The logs made this visible:

- before fix: `timeouts=81`
- before fix: `actual_ms≈2417`
- after fix: `timeouts=0`
- after fix: `actual_ms=1293`

This was the root cause of the "slow playback" issue.

## Final Fix

The successful fix was deliberately small and local to `firmware`.

### Code changes

- Added `BBCLAW_AUDIO_IO_TIMEOUT_MS=200` in [bb_config.h](/Volumes/1TB/github/bbclaw/firmware/include/bb_config.h)
- Replaced the old tick-based timeout argument in [bb_audio.c](/Volumes/1TB/github/bbclaw/firmware/src/bb_audio.c) with the real millisecond value for all `i2s_channel_read/write()` calls
- Kept playback rate compensation at `1/1`
- Kept the experiment framework and unified playback diagnostics so future regressions are observable

### Final working playback shape

The final working configuration remained:

- TX experiment: `BBCLAW_AUDIO_TX_EXPERIMENT=1`
- TX config: `stereo32`
- app write shape: stereo pair `[v,v]`
- compensation: `1/1`

This is important: the TX format itself was not the root bug. The timeout handling was.

## Validation

### Boot WAV

From the final successful log:

- `i2s prep=1 ... tx_cfg=stereo32 ... comp=1/1 exp=1`
- `boot wav play len=41410 rate=16000 ch=1 bits=16`
- `play_pcm stereo32 samples=20705 i2s_bytes=165640 elapsed_ms=1291 timeouts=0`
- `boot wav done expected_ms=1294 actual_ms=1293 ratio_x100=99 err=ESP_OK`

Result:

- passes timing target
- audible
- no timeout inflation

### TTS

From the same final log:

- `play_pcm stereo32 samples=63360 i2s_bytes=506880 elapsed_ms=3939 timeouts=0`

At `16000 Hz`, `63360` mono samples should take about `3960 ms`, so this is effectively correct.

Result:

- TTS timing is normal again

### RX capture regression check

From the final log:

- `capture summary frames=133 bytes=68096 timeouts=0`

Result:

- capture path remained healthy
- no new timeout or resource conflict regression was introduced

## What Misled Us

Several factors made this bug look like a format or clocking problem:

- slow playback ratio was stable and looked like a sample-rate error
- some simplex experiments removed GPIO conflicts but still had bad timing
- changing the compensation ratio changed the symptom, which made clocking look more guilty than it really was
- `stereo32` had previously been associated with "audible but slow", while `stereo16` had produced "effectively silent" behavior in one failed shape

The key debugging mistake to avoid next time:

- do not assume new ESP-IDF peripheral driver APIs use FreeRTOS tick units just because older driver patterns often did

## Guardrails

To keep this from recurring:

1. Keep the unified playback diagnostics:
   - `play_pcm ... elapsed_ms=... timeouts=...`
   - `boot wav done expected_ms=... actual_ms=... ratio_x100=...`
2. Keep `firmware/assets/svg/bbclaw.wav` as the golden local playback sample.
3. Treat any non-zero repeated playback timeout count as a first-class bug signal.
4. Do not use playback rate compensation as a real fix unless the hardware clocking issue is independently proven.
5. When moving between legacy and new ESP-IDF drivers, verify timeout units explicitly against official docs.

## Scope And Upstream Explainability

This change belongs to the `firmware` layer and fixes the local device playback path.

It is explainable in upstream terms because it is not a BBClaw-only special case:

- it corrects misuse of the ESP-IDF I2S channel API timeout units
- it adds clearer diagnostics for playback timing and timeout behavior

That makes it a defensible bugfix, not a product-specific workaround.
