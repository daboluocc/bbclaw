---
name: bench-serial
description: "Use when debugging or driving a BBClaw *bench* board over its UART0 serial console — capturing ESP_LOG output, injecting navigation keys / PTT via text commands, checking heap watermarks, or running a code→build→flash→UART-verify loop on a board that has NO TinyUSB CDC1 (so device-monitor screenshots are impossible). Triggers: \"抓串口日志\", \"串口\", \"UART\", \"注入按键\", \"模拟按键\", \"查堆\", \"看堆\", \"heap\", \"bench 板\", \"调试板\", \"serial log\", \"inject key\", \"capture serial\", \"xTaskCreate 失败\", \"任务栈\", \"site fetch\", \"PTT 自测\". NOT for production PCBs with CH334F hub + CDC1 — that's the device-monitor skill (screenshots + binary key protocol)."
---

# Bench Serial — UART Console Capture + Text Command Injection

Drive and observe a BBClaw **bench debug board** entirely over its single UART0
console. The bench board has no TinyUSB CDC1, so the binary screenshot/key
protocol of the **device-monitor** skill does not exist here. Instead, the
firmware exposes a plain-text, newline-terminated command channel on UART0
(`firmware/src/bb_uart_cmd.c`, gated by `CONFIG_BBCLAW_DEVICE_MONITOR`) sharing
the same wire as `ESP_LOG`. You inject keys/PTT as text, then read the log to
verify — there is **no way to take a screenshot** on this board.

## bench-serial vs device-monitor — pick the right skill

| | **bench-serial** (this skill) | **device-monitor** |
|---|---|---|
| Hardware | Bench debug board (CH340 only) | Production BBClaw PCB w/ CH334F USB hub |
| Channel | UART0 text console (115200) | TinyUSB **CDC1** binary protocol (460800) |
| Screenshots | ❌ impossible (no framebuffer channel) | ✅ `lv_snapshot_take` → PNG |
| Key inject | ✅ `key ...` text command | ✅ binary `REQ_INPUT` frame |
| PTT / heap | ✅ `ptt`, `heap` text commands | ❌ not exposed |
| Verify by | Reading ESP_LOG over UART | Comparing PNG before/after |

If the user is on a **production PCB** and wants screenshots / visual UI
iteration → use **device-monitor**, not this skill.

## Hardware / port layout (bench board)

Only **two** ports enumerate on macOS — there is no `usbmodem*3` (CDC1):

```bash
ls /dev/cu.usb*
# /dev/cu.usbserial-21130   ← CH340 → UART0.  USE THIS for everything:
#                              esptool flashing (460800) AND
#                              ESP_LOG console + text command injection (115200)
# /dev/cu.usbmodem2112401   ← CDC0, basically silent — DO NOT use
```

Auto-discover the console port:
```bash
PORT=$(ls /dev/cu.usbserial-* 2>/dev/null | head -1)
```
The helper scripts default to this same auto-discovery, so usually you can omit
`--port` entirely.

## UART text command set

Truth source: `firmware/src/bb_uart_cmd.c`. Commands are **newline-terminated
plain text** sent to UART0 @ 115200, case-insensitive. Replies come back as
normal `ESP_LOG` lines (tag `bb_uart_cmd`).

| Command | Effect |
|---|---|
| `key up` / `down` / `left` / `right` / `ok` / `back` / `ok-long` | Inject one navigation key event (→ `bb_nav_input_inject`) |
| `ptt down` / `ptt up` / `ptt tap [ms]` | Inject PTT edges; `tap` holds then releases (default 200 ms) |
| `heap` | Print `internal_free / internal_largest / spiram_free / spiram_largest / total_free` + full internal-region breakdown |
| `help` | List the command set |

## Helper scripts (`firmware/scripts/`)

Standard-library + pyserial only.

| Script | What it does |
|---|---|
| `uartcmd_capture.py` | Open UART0, read for N seconds via a reader thread, print (optionally regex-filtered) lines. Pure observation. |
| `uartcmd_inject.py` | Send a sequence of text commands (paced so UI animations/fetches settle), echoing everything the device logs back. The driving tool. |

Both auto-discover `/dev/cu.usbserial-*`; pass `--port` to override.

### uartcmd_capture.py

```bash
# 10s of everything
python3 firmware/scripts/uartcmd_capture.py --seconds 10

# only connection-related lines
python3 firmware/scripts/uartcmd_capture.py --seconds 12 --grep "site fetch|adapter|WS|wifi"

# live stream instead of buffer-then-filter
python3 firmware/scripts/uartcmd_capture.py --seconds 8 --raw
```
Flags: `--port`, `--baud` (115200), `--seconds`, `--grep <regex>`, `--raw`.

### uartcmd_inject.py

```bash
# from standby: OK to enter, scroll down, OK into submenu, then read heap
python3 firmware/scripts/uartcmd_inject.py key ok key down key ok heap

# multi-word command: quote it, or use --cmds with ';'
python3 firmware/scripts/uartcmd_inject.py "ptt tap 500" heap
python3 firmware/scripts/uartcmd_inject.py --cmds "key ok; key down; key ok"

# slower pacing + longer tail for async fetch logs
python3 firmware/scripts/uartcmd_inject.py --gap 1.2 --tail 4 key ok
```
Flags: `--port`, `--baud`, `--gap` (per-command settle, default 0.9 s),
`--tail` (extra read window after last command, default 2 s), `--cmds`, plus
positional command words. It warns on bad verbs/args but still sends them.

## Typical workflows

### A. Capture boot logs (WiFi / WS / adapter connect)

Connection logs **only appear during boot** — an idle board has long passed
them. RTS cannot reboot this board (see pitfall 1), so trigger a real reset
first, then capture immediately:

```bash
PORT=$(ls /dev/cu.usbserial-* | head -1)
# Re-flash resets the chip; start capture right after it begins booting:
make flash PORT=$PORT
python3 firmware/scripts/uartcmd_capture.py --port $PORT --seconds 15 \
  --grep "wifi|WS|adapter|site fetch|boot"
# (or physically press RESET, then run the capture)
```

### B. Drive the UI into Settings via key injection

Miyu (密语) is off by default → device sits in CHAT/standby. `key ok` from
standby opens Settings (which kicks off driver/sites fetch). Navigate with
`key down`/`key up` (lists wrap), `key ok` to enter a submenu, `key back` to
leave.

```bash
python3 firmware/scripts/uartcmd_inject.py --gap 1.0 --tail 4 \
  key ok key down key ok
# watch the echoed log for "inject nav key=..." + any "site fetch"/driver lines
```

### C. Diagnose `xTaskCreate` / task-stack failures (heap watermarks)

When a task fails to spawn (e.g. WS sites task, TTS, OTA-AES — internal DRAM is
the recurring culprit, see MEMORY internal-dram-recurring-root-cause), read the
heap before/after the action:

```bash
python3 firmware/scripts/uartcmd_inject.py heap                 # baseline
python3 firmware/scripts/uartcmd_inject.py key ok heap          # after entering Settings
# compare internal_free / internal_largest — a large drop or a largest-block
# below the task's stack size explains the create failure. spiram_free is the
# fix target: move the stack to PSRAM (MALLOC_CAP_SPIRAM), as bb_uart_cmd.c does.
```

### D. Full code → build → flash → UART-verify loop

No screenshots here, so verification is reading the log after re-driving the UI:

```bash
PORT=$(ls /dev/cu.usbserial-* | head -1)
# 1. edit firmware (Edit tool)
# 2. build
cd firmware && idf.py build 2>&1 | tail -3
# 3. flash (resets the board — bench board has no software reboot path)
make flash PORT=$PORT
# 4. re-drive the UI and capture the new behavior
python3 scripts/uartcmd_inject.py --gap 1.0 --tail 4 key ok key down key ok
# 5. or just observe a window
python3 scripts/uartcmd_capture.py --port $PORT --seconds 12 --grep "<your tag>"
```

## Pitfalls (learned this session — all verified)

1. **RTS does NOT reset this board.** No EN auto-reset transistor is wired on
   the CH340 bench board. pyserial toggling RTS is a no-op (device keeps
   printing idle logs, never reboots). `bb_uart_cmd` has **no reboot command**.
   To restart from boot: `make flash` (resets) or physically press RESET.
2. **Idle board only prints `bb_button_test: gpioX stable raw=N`** (~1 line /
   2 s). That is a background diagnostic task — normal, not a hang. WiFi/WS/
   adapter logs are boot-only; to see them you must reboot and capture from t=0
   (Workflow A).
3. **`key ok` from standby opens Settings** (miyu off by default → CHAT/standby
   state). Settings entry triggers driver/sites fetch. Lists wrap on
   `down`/`up`; `back` exits.
4. **Persisted log file** `firmware/.cache/idf-monitor.latest.log` only exists
   if `make monitor` has run. For bench debugging, the pyserial capture script
   above is more reliable than depending on that file.
5. **`timeout` is not available** in the default macOS shell — never use it.
   The scripts use Python timing loops instead; do the same in any ad-hoc code.

## Related

- **device-monitor skill** (`.claude/skills/device-monitor/SKILL.md`) — the CDC1
  screenshot/binary-key path for production PCBs. Mutually exclusive hardware.
- **MEMORY** `serial-log-viewing` — persisted log file convention; no front-foreground
  `make monitor`.
- **MEMORY** `bench-uart-key-injection` — bench key self-test goes through UART
  text commands, not CDC1; GPIO debounce can't be tested this way.
- **MEMORY** `bbclaw-bench-flash-port` — flash over `/dev/cu.usbserial-*`; the
  `usbmodem0010000001` decoy device must not be flashed.
- **MEMORY** `internal-dram-recurring-root-cause` — internal DRAM pressure is the
  recurring crash cause; the `heap` command (Workflow C) is how you confirm it.

## Files referenced

| Path | Purpose |
|---|---|
| `firmware/src/bb_uart_cmd.c` | Firmware: UART text command parser (truth for command set) |
| `firmware/include/bb_uart_cmd.h` | `bb_uart_cmd_start()` API |
| `firmware/include/bb_nav_input.h` | `bb_nav_input_inject(event)` — what `key` drives |
| `firmware/scripts/uartcmd_capture.py` | Host: capture/filter UART console output |
| `firmware/scripts/uartcmd_inject.py` | Host: inject text commands + capture replies |
