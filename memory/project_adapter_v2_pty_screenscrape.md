---
name: project_adapter_v2_pty_screenscrape
description: Why adapter_v2 drives interactive claude via PTY+screen-scrape instead of `claude -p` — billing + multi-CLI
metadata:
  type: project
---

adapter_v2 deliberately drives the **interactive** claude TUI over a persistent PTY and screen-scrapes the rendered output (`vtscreen` emulator → `extract` package), rather than `claude -p --output-format stream-json` (the clean structured path v1 used).

**Why:** (1) **计费独立性** — `claude -p` (headless) may be billed as a separate per-call API scheme; driving the interactive claude keeps every device turn on the user's interactive **subscription** billing, not a separate paid path. (2) **多 CLI 无缝兼容** — scraping a human-facing TUI is CLI-agnostic, so any future agent CLI that renders to a terminal works with the same `vtscreen`+`extract` pipeline, no per-CLI structured driver.

**How to apply:** Treat the brittle `extract` layer as load-bearing, not tech debt — don't "just switch to `-p` stream-json" to fix extraction bugs without re-checking the billing constraint. Every extraction branch needs a case in [[project_adapter_v2_pty_screenscrape]]'s catalog `adapter_v2/internal/extract/CASES.md`. Decision recorded as ADR-035; the long-reply scroll-off TTS-truncation bug (fixed 2026-06-25 via scrollback-aware extraction) is a known side-effect of this choice.
