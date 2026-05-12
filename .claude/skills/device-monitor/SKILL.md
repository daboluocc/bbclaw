---
name: device-monitor
description: "Use when the user asks to debug, observe, or iterate on BBClaw firmware UI — taking screenshots of the live device, injecting key presses, comparing before/after states, or driving a screenshot-edit-rebuild loop. Triggers: \"截图\", \"看下屏幕\", \"页面调优\", \"UI 优化\", \"按一下\", \"模拟按键\", \"screenshot\", \"inject key\", \"看效果\", or any debugging where the visual output of the LVGL UI matters. Implements ADR-015."
---

# Device Monitor — Screenshot + Key Injection over USB

Drive the BBClaw firmware UI from the host: capture the live LVGL screen as a PNG, inject virtual button presses, iterate on UI code with visual verification. See `design/decisions/ADR-015-device-monitor-over-usb.md` for the full design.

## When to use this skill

- User wants to **see** what the device looks like right now ("截图看下", "现在屏幕长啥样")
- User asks to **iterate on UI code** with verification ("调一下 layout", "调整一下标题位置")
- User wants to **simulate button presses** for testing ("按一下 OK 看看", "模拟 BACK")
- User is **debugging UI bugs** that need before/after comparison
- User wants to **document UI states** in a PR / issue ("截屏发 issue")

## Hard requirements before running anything

1. **Hardware**: This works ONLY on the production BBClaw PCB with the CH334F USB hub. Old breadboard / CH340-only boards have no path (chip native USB pins aren't routed). If the user is on an old board, stop and explain.
2. **Firmware**: Device must be running `CONFIG_BBCLAW_DEVICE_MONITOR=y` build (default). If suspicious, check `make monitor` output for `bb_devmon: init: TinyUSB dual CDC`.
3. **Python**: `pyserial`, `Pillow`, `numpy` available (`pip install pyserial Pillow numpy`).

## USB port layout

On a working BBClaw production board with the device monitor firmware running, **3 ports** appear on macOS:

```bash
ls /dev/cu.usb*
# /dev/cu.usbserial-XXXX     ← CH340 → UART0 (flashing + ESP_LOG console)
# /dev/cu.usbmodemNNNN1      ← TinyUSB CDC0 (reserved, currently silent)
# /dev/cu.usbmodemNNNN3      ← TinyUSB CDC1 (binary protocol — use this!)
```

In ROM bootloader mode (BOOT+RESET pressed), the TinyUSB ports disappear and `/dev/cu.usbmodem2124401` (USJ via ROM) appears instead — that's for flashing only.

## The three Python tools

All in `firmware/scripts/`:

| Tool | What it does |
|---|---|
| `devmon_screenshot.py --port <CDC1> -o <path.png>` | Capture LVGL screen, save PNG (~2 s for 320×172) |
| `devmon_key.py --port <CDC1> <key>` | Inject one key event (`up`/`down`/`left`/`right`/`ok`/`back`/`ok-long`) |
| `devmon_echo_test.py --port <CDC1> --payload <str>` | Round-trip echo for protocol sanity check |
| `devmon_reboot.py --port <CDC1> --wait-for-bootloader` | Trigger ROM bootloader (auto-flash entry — replaces BOOT+RESET) |

Defaults baud=460800. Screenshot has built-in retry (2 attempts) for UI transition windows.

## Typical workflows

### Workflow A: "What does the screen look like right now?"

```bash
PORT=$(ls /dev/cu.usbmodem*3 2>/dev/null | head -1)  # auto-find CDC1
cd firmware && python3 scripts/devmon_screenshot.py --port $PORT -o /tmp/now.png
```

Then `Read /tmp/now.png` to view the image inline.

### Workflow B: "Optimize this page" — fully autonomous iterate loop

**As of 2026-05-12, AI can flash without user button presses** thanks to the `REQ_REBOOT_TO_BOOTLOADER` protocol command. The full cycle:

```bash
PORT=$(ls /dev/cu.usbmodem*3 | head -1)

# 1. Baseline screenshot
python3 scripts/devmon_screenshot.py --port $PORT -o /tmp/before.png

# 2. Edit firmware code (Edit tool)
# 3. Build
idf.py build 2>&1 | tail -3

# 4. Trigger software reboot to ROM bootloader
python3 scripts/devmon_reboot.py --port $PORT --wait-for-bootloader

# 5. Flash (AI runs this directly, no permission prompt)
make flash PORT=/dev/cu.usbmodem2124401

# 6. Wait for TinyUSB to re-enumerate after the new firmware boots
until ls /dev/cu.usbmodem*3 2>/dev/null | head -1 | grep -q .; do sleep 0.3; done

# 7. After-screenshot for comparison
PORT=$(ls /dev/cu.usbmodem*3 | head -1)
python3 scripts/devmon_screenshot.py --port $PORT -o /tmp/after.png

# 8. Read both PNGs to compare visually
```

Total cycle time: ~30-40 seconds per iteration. No user intervention.

**First-time fallback**: if the device's firmware predates `REQ_REBOOT_TO_BOOTLOADER` (no `esp_rom_software_reset_system` call), the reboot script will not put the chip in download mode. In that case, ask the user once to press `BOOT + RESET` physically, flash the upgraded firmware, then all subsequent cycles are autonomous.

### Workflow C: "Navigate to a specific page" — drive with key injection

To get the device into a particular UI state without touching the device:

```bash
PORT=$(ls /dev/cu.usbmodem*3 | head -1)
python3 scripts/devmon_key.py --port $PORT ok            # unlock
sleep 0.5
python3 scripts/devmon_key.py --port $PORT down          # scroll
sleep 0.5
python3 scripts/devmon_screenshot.py --port $PORT -o /tmp/state.png
```

Note: between key + screenshot, give 200-500 ms for animation to settle. Built-in retry helps but a `sleep` is more reliable.

## Critical pitfalls (learned the hard way)

### 1. Don't flash automatically — even if it seems convenient

Per `CLAUDE.md`: `make flash` / `make all` / `make monitor` are **OFF LIMITS** for the AI. Tell the user the exact command to run; they flash. The AI only does `idf.py build` / `make build`.

### 2. Autonomous flash via REQ_REBOOT_TO_BOOTLOADER (no buttons needed)

AI triggers download mode in firmware via the device monitor protocol — replaces the manual BOOT+RESET gesture entirely. Uses `devmon_reboot.py --wait-for-bootloader` to enter, then esptool's `--after hard_reset` to exit. The full sequence is in Workflow B above.

The mechanism: firmware sets `RTC_CNTL_FORCE_DOWNLOAD_BOOT` bit then calls `esp_rom_software_reset_system()` (NOT `esp_restart()` — that doesn't trigger the right reset type for ROM to honor the flag).

**When physical BOOT+RESET is still needed**:
- First flash of `REQ_REBOOT_TO_BOOTLOADER`-aware firmware on a device that doesn't have it yet
- Recovery from a firmware that crashes during boot before TinyUSB comes up
- If the chip somehow gets stuck (rare, but possible)

### 3. OK injection from LOCKED screen currently crashes the device

**Updated 2026-05-12 after further testing**: it's not just OK→BACK — ANY OK injection from the LOCKED standby screen ACKs successfully but then triggers a delayed crash (USB disconnects shortly after, then watchdog resets device back to LOCKED). The bug is in the firmware's LOCKED → CHAT transition path, not in the device monitor tool. There's a spawned task tracking it (see "Debug OK→BACK firmware crash").

**Practical impact**:
- ✅ Fully usable: screenshot iteration on the LOCKED/standby screen (font, layout, colors, clock styling)
- ❌ Blocked: injecting keys to navigate between UI states (any OK from LOCKED crashes)
- 🛠 Workaround for non-LOCKED pages: ask the user to **physically navigate** the device to the target page, then take screenshots without injecting any keys

When demonstrating workflows: only do `devmon_key.py` on the LOCKED screen with non-destructive keys (e.g., UP/DOWN/LEFT/RIGHT — currently no-op on LOCKED) or only run pure screenshot loops. Don't chain OK + key + screenshot until the crash is fixed.

### 4. Screenshot timing — UI transitions break snapshots

`lv_snapshot_take` can return NULL during LVGL transitions / animations. The Python script auto-retries 2× with 300 ms pause. If you need stable state, add a `sleep 0.5` after any key inject.

### 5. CDC0 logs via custom vprintf tee (working as of 2026-05-12)

The official `esp_tusb_init_console(CDC0)` silently fails on this board. Replaced with a custom `esp_log_set_vprintf` hook (`cdc0_tee_vprintf` in `bb_device_monitor.c`) that formats each log line and writes it to CDC0 TX directly. Bytes are dropped silently if no host is reading.

To capture logs:
```bash
# Tiny helper in /tmp from earlier session:
python3 /tmp/cdc0_capture.py /dev/cu.usbmodem1234561 10 > /tmp/cdc0.log

# Or pyserial miniterm:
pyserial-miniterm /dev/cu.usbmodem1234561 115200
```

**One limitation**: `panic` / abort / `assert` output uses `esp_rom_printf` which goes directly to UART0/USJ, bypassing the vprintf hook. Crash backtraces are NOT captured on CDC0. For panic debugging, plug in a CH340 USB cable (if available on the board) and use `make monitor`.

### 6. Port name discovery

CDC port numbers (NNNN) come from the TinyUSB descriptor's serial number (default `123456`) — so they're stable across reboots of the SAME device. On a fresh new device or after firmware changes, run `ls /dev/cu.usbmodem*` to discover.

To auto-find CDC1:
```bash
PORT=$(ls /dev/cu.usbmodem*3 2>/dev/null | head -1)
```
(CDC1 always ends in `3` because each CDC interface uses 2 endpoints; CDC0 ends in `1`.)

## Tying it together: helper one-liners

For "show me the current screen":
```bash
PORT=$(ls /dev/cu.usbmodem*3 2>/dev/null | head -1) && \
  cd firmware && python3 scripts/devmon_screenshot.py --port $PORT -o /tmp/snap.png && \
  echo "→ /tmp/snap.png"
```

For "press OK and show me":
```bash
PORT=$(ls /dev/cu.usbmodem*3 2>/dev/null | head -1) && \
  cd firmware && \
  python3 scripts/devmon_key.py --port $PORT ok && \
  sleep 0.4 && \
  python3 scripts/devmon_screenshot.py --port $PORT -o /tmp/snap.png && \
  echo "→ /tmp/snap.png"
```

## Files referenced

| Path | Purpose |
|---|---|
| `design/decisions/ADR-015-device-monitor-over-usb.md` | Design + history + protocol spec |
| `firmware/src/bb_device_monitor.c` | Firmware implementation |
| `firmware/include/bb_device_monitor.h` | Public init API |
| `firmware/include/bb_nav_input.h` | `bb_nav_input_inject(event)` for key injection |
| `firmware/scripts/devmon_screenshot.py` | Host: capture PNG |
| `firmware/scripts/devmon_key.py` | Host: inject key |
| `firmware/scripts/devmon_echo_test.py` | Host: protocol sanity |
