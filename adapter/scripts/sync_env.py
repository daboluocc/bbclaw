#!/usr/bin/env python3
"""
sync_env.py — scan local environment, write a ready-to-run adapter/.env.

Probes each agent driver and fills only the env vars that the driver actually
needs to override its built-in defaults:

  openclaw     ~/.openclaw/openclaw.json     OPENCLAW_AUTH_TOKEN [+ OPENCLAW_WS_URL if non-default port]
  claude-code  `claude` on PATH               (no env needed — auto-registered)
  opencode     `opencode` on PATH             (no env needed — auto-registered)
  ollama       127.0.0.1:11434 reachable      OLLAMA_MODEL = first installed
  aider        `aider` on PATH                (no env needed — auto-registered)

Optional Volcano-ASR key import (--doubao-env):
  If you happen to have another project with Volcano/Doubao ASR keys in a
  .env file, point at it and the script will pull the credentials over,
  translating field names to bbclaw's convention:
    ASR_TOKEN     → ASR_API_KEY
    ASR_CLUSTER   → (informational; warns if not bigmodel)
    ASR_APP_ID    → ASR_APP_ID
    TTS_TOKEN     → TTS_TOKEN
    TTS_APP_ID    → TTS_APP_ID
  This step is opt-in only; the script never reads files you didn't ask for.

By default merges into an existing adapter/.env (keys you've already set are
preserved). Use --reset to discard the file and write fresh.

Usage:
    python3 scripts/sync_env.py
    python3 scripts/sync_env.py --dry-run
    python3 scripts/sync_env.py --reset
    python3 scripts/sync_env.py --doubao-env <path>
"""
from __future__ import annotations

import argparse
import json
import os
import shutil
import socket
import subprocess
import sys
from collections import OrderedDict
from datetime import datetime
from pathlib import Path

ROOT = Path(__file__).resolve().parents[1]
ENV_PATH = ROOT / ".env"
OPENCLAW_CONFIG = Path.home() / ".openclaw" / "openclaw.json"
OLLAMA_DEFAULT_HOST = ("127.0.0.1", 11434)

DEFAULT_OPENCLAW_PORT = 18789  # adapter's built-in default; only emit URL if mismatch


# ─── detection helpers ─────────────────────────────────────────────────────


def which(bin_name: str) -> str | None:
    return shutil.which(bin_name)


def tcp_open(host: str, port: int, timeout: float = 0.4) -> bool:
    try:
        with socket.create_connection((host, port), timeout=timeout):
            return True
    except OSError:
        return False


def detect_openclaw() -> dict:
    """Read ~/.openclaw/openclaw.json. Return {} if not found / unreadable."""
    if not OPENCLAW_CONFIG.is_file():
        return {"present": False}
    try:
        cfg = json.loads(OPENCLAW_CONFIG.read_text())
    except (OSError, json.JSONDecodeError) as e:
        return {"present": False, "error": str(e)}
    gateway = cfg.get("gateway") or {}
    port = gateway.get("port") or DEFAULT_OPENCLAW_PORT
    auth = (gateway.get("auth") or {}).get("token") or ""
    return {
        "present": True,
        "config": OPENCLAW_CONFIG,
        "port": port,
        "token": auth,
    }


def detect_claude_code() -> dict:
    """Just check the binary on PATH. Credentials live in many places
    (keychain on macOS, ~/.claude/credentials, ANTHROPIC_API_KEY env, …) and
    chasing them all produces false negatives. The driver itself raises a
    clean error at first turn if creds are missing — that's good enough."""
    bin_path = which("claude")
    return {"present": bin_path is not None, "bin": bin_path}


def detect_opencode() -> dict:
    bin_path = which("opencode")
    return {"present": bin_path is not None, "bin": bin_path}


def detect_aider() -> dict:
    bin_path = which("aider")
    return {"present": bin_path is not None, "bin": bin_path}


def detect_doubao(source_path: Path | None) -> dict:
    """Read a sales-apis-style .env and translate Volcano ASR/TTS keys to
    bbclaw adapter's naming convention. The source file is untracked,
    user-specific; we only read it if the user explicitly points at one
    (CLI arg or DOUBAO_ENV_FILE)."""
    if source_path is None:
        return {"present": False, "reason": "no source path (pass --doubao-env or set DOUBAO_ENV_FILE)"}
    if not source_path.is_file():
        return {"present": False, "reason": f"{source_path} not a file"}
    try:
        raw = parse_env(source_path)
    except OSError as e:
        return {"present": False, "reason": str(e)}

    # Map sales-apis style → bbclaw adapter style.
    out: dict[str, str] = {}
    if v := raw.get("ASR_APP_ID"):
        out["ASR_APP_ID"] = v
    if v := raw.get("ASR_TOKEN") or raw.get("ASR_API_KEY"):
        out["ASR_API_KEY"] = v
    cluster = raw.get("ASR_CLUSTER", "")
    if v := raw.get("TTS_APP_ID"):
        out["TTS_APP_ID"] = v
    if v := raw.get("TTS_TOKEN"):
        out["TTS_TOKEN"] = v

    return {
        "present": bool(out),
        "source": source_path,
        "mapped": out,
        "cluster_hint": cluster,
        "raw_provider": raw.get("ASR_PROVIDER", ""),
    }


def detect_ollama() -> dict:
    if not tcp_open(*OLLAMA_DEFAULT_HOST):
        return {"present": False, "reason": f"{OLLAMA_DEFAULT_HOST[0]}:{OLLAMA_DEFAULT_HOST[1]} not listening"}
    # Try to list installed models. ollama CLI is the easiest source.
    bin_path = which("ollama")
    models: list[str] = []
    if bin_path:
        try:
            out = subprocess.run(
                [bin_path, "list"], capture_output=True, text=True, timeout=3, check=False
            )
            for line in out.stdout.splitlines()[1:]:  # skip header
                cols = line.split()
                if cols:
                    models.append(cols[0])
        except (subprocess.SubprocessError, OSError):
            pass
    return {"present": True, "bin": bin_path, "models": models}


# ─── env file plumbing ─────────────────────────────────────────────────────


def parse_env(path: Path) -> "OrderedDict[str, str]":
    out: OrderedDict[str, str] = OrderedDict()
    if not path.is_file():
        return out
    for raw in path.read_text().splitlines():
        line = raw.strip()
        if not line or line.startswith("#"):
            continue
        if "=" not in line:
            continue
        k, _, v = line.partition("=")
        out[k.strip()] = v.strip()
    return out


def render_env(values: dict[str, str], detected: dict) -> str:
    """Pretty-print a managed adapter/.env with section comments."""
    lines: list[str] = []
    lines.append("# adapter/.env — generated by scripts/sync_env.py")
    lines.append(f"# Last sync: {datetime.now().isoformat(timespec='seconds')}")
    lines.append("# Re-run `make sync-env` after installing/uninstalling a driver locally.")
    lines.append("")

    def emit(comment: str, keys: list[str]):
        # Only emit a section if at least one key in it has a non-empty value.
        # Empty placeholders just clutter the file; users reach for .env.example
        # when they want to discover knobs.
        any_present = any(values.get(k) for k in keys)
        if not any_present:
            return
        lines.append(comment)
        for k in keys:
            if values.get(k):
                lines.append(f"{k}={values[k]}")
        lines.append("")

    emit("# OpenClaw gateway (auto-extracted from ~/.openclaw/openclaw.json)",
         ["OPENCLAW_AUTH_TOKEN", "OPENCLAW_WS_URL"])
    emit("# Default agent driver (heuristic: first detected)",
         ["AGENT_DEFAULT_DRIVER"])
    emit("# Ollama (auto-detected installed model)",
         ["OLLAMA_MODEL"])
    emit("# Voice path — ASR + TTS providers. Replace placeholders with real\n# keys to enable PTT (see .env.example for full provider docs).",
         ["ASR_PROVIDER", "ASR_LOCAL_BIN", "ASR_LOCAL_TEXT_PATH", "ASR_READINESS_PROBE",
          "ASR_APP_ID", "ASR_API_KEY", "ASR_BASE_URL", "ASR_MODEL",
          "TTS_PROVIDER", "TTS_APP_ID", "TTS_TOKEN"])
    emit("# Cloud relay (set both to enable cloud_saas firmware support)",
         ["CLOUD_WS_URL", "CLOUD_AUTH_TOKEN"])
    emit("# Production hardening",
         ["ADAPTER_AUTH_TOKEN"])

    # Tail: anything we didn't explicitly group
    grouped = {
        "OPENCLAW_AUTH_TOKEN", "OPENCLAW_WS_URL", "AGENT_DEFAULT_DRIVER",
        "OLLAMA_MODEL",
        "ASR_PROVIDER", "ASR_LOCAL_BIN", "ASR_LOCAL_TEXT_PATH", "ASR_READINESS_PROBE",
        "ASR_APP_ID", "ASR_API_KEY", "ASR_BASE_URL", "ASR_MODEL",
        "TTS_PROVIDER", "TTS_APP_ID", "TTS_TOKEN",
        "CLOUD_WS_URL", "CLOUD_AUTH_TOKEN", "ADAPTER_AUTH_TOKEN",
    }
    extras = [(k, v) for k, v in values.items() if k not in grouped]
    if extras:
        lines.append("# Other (preserved from your previous .env)")
        for k, v in extras:
            lines.append(f"{k}={v}")
        lines.append("")

    return "\n".join(lines).rstrip() + "\n"


# ─── orchestration ─────────────────────────────────────────────────────────


def pick_default_driver(detected: dict) -> str | None:
    # Preference order — claude-code first if both PATH and creds present, else
    # whatever else self-registers.
    prefs = [
        ("claude-code", detected["claude-code"]["present"]),
        ("opencode", detected["opencode"]["present"]),
        ("aider", detected["aider"]["present"]),
        ("openclaw", detected["openclaw"]["present"]),
        ("ollama", detected["ollama"]["present"]),
    ]
    for name, ok in prefs:
        if ok:
            return name
    return None


def main(argv: list[str] | None = None) -> int:
    ap = argparse.ArgumentParser(description=__doc__,
                                 formatter_class=argparse.RawDescriptionHelpFormatter)
    ap.add_argument("--dry-run", action="store_true",
                    help="print the new .env to stdout, do not touch the file")
    ap.add_argument("--reset", action="store_true",
                    help="discard existing .env, write a fresh one (backup first)")
    ap.add_argument("--doubao-env", default=os.environ.get("DOUBAO_ENV_FILE"),
                    help="path to a sales-apis-style .env to import Doubao/Volcano "
                         "ASR/TTS keys from (env var: DOUBAO_ENV_FILE)")
    args = ap.parse_args(argv)

    doubao_src = Path(args.doubao_env).expanduser() if args.doubao_env else None

    detected = {
        "openclaw": detect_openclaw(),
        "claude-code": detect_claude_code(),
        "opencode": detect_opencode(),
        "aider": detect_aider(),
        "ollama": detect_ollama(),
    }
    # Doubao is opt-in only — never list it unless the user pointed at a source.
    if doubao_src is not None:
        detected["doubao"] = detect_doubao(doubao_src)

    print("Driver scan:")
    for name, info in detected.items():
        if info["present"]:
            tag = "✓"
            extra = ""
            if name == "openclaw":
                tok = info.get("token") or ""
                extra = f"  port={info['port']}  token={tok[:8]}…" if tok else f"  port={info['port']}  token=(missing)"
            elif name == "ollama":
                models = info.get("models") or []
                extra = f"  models={','.join(models[:3]) or '(none installed)'}"
            elif name == "doubao":
                keys = list(info.get("mapped", {}).keys())
                extra = f"  from {info['source']}  keys={','.join(keys) or '(none)'}"
            elif info.get("bin"):
                extra = f"  {info['bin']}"
        else:
            tag = "✗"
            extra = "  " + (info.get("reason") or info.get("error") or "not found")
        print(f"  {tag} {name:<12}{extra}")

    # Build the values dict — start from existing .env (unless --reset)
    if args.reset or not ENV_PATH.is_file():
        values: OrderedDict[str, str] = OrderedDict()
    else:
        values = parse_env(ENV_PATH)

    def set_if_unset(key: str, val: str | None) -> None:
        # Treat empty string as unset — user reset .env or the line is a
        # placeholder. Only preserve a value when the user actually filled it.
        if val and (key not in values or not values[key]):
            values[key] = val

    # OpenClaw — only set if we read a real token. URL only when port differs from default.
    if detected["openclaw"]["present"] and detected["openclaw"].get("token"):
        set_if_unset("OPENCLAW_AUTH_TOKEN", detected["openclaw"]["token"])
        port = int(detected["openclaw"]["port"])
        if port != DEFAULT_OPENCLAW_PORT:
            set_if_unset("OPENCLAW_WS_URL", f"ws://127.0.0.1:{port}")

    # Pick a default driver
    default_driver = pick_default_driver(detected)
    if default_driver:
        set_if_unset("AGENT_DEFAULT_DRIVER", default_driver)

    # Ollama model
    if detected["ollama"]["present"] and detected["ollama"].get("models"):
        set_if_unset("OLLAMA_MODEL", detected["ollama"]["models"][0])

    # Doubao import (--doubao-env): translate sales-apis-style ASR keys.
    # Promote the imported keys into our values dict before falling back to
    # placeholder providers, so a Doubao-enabled run wins over the mock path.
    if detected.get("doubao", {}).get("present"):
        for k, v in detected["doubao"]["mapped"].items():
            set_if_unset(k, v)
        if "ASR_APP_ID" in values and "ASR_API_KEY" in values:
            # Override any leftover ASR_PROVIDER=local (placeholder) — we have
            # real Doubao credentials now.
            values["ASR_PROVIDER"] = "doubao_native"
            for k in ("ASR_LOCAL_BIN", "ASR_LOCAL_TEXT_PATH", "ASR_READINESS_PROBE"):
                values.pop(k, None)
        if "TTS_APP_ID" in values and "TTS_TOKEN" in values:
            values["TTS_PROVIDER"] = "doubao_native"

    # ASR/TTS placeholders so the adapter passes config.Validate() with zero
    # voice configuration. /v1/stream/* will fail with a clean error at runtime
    # (intended — no ASR/TTS keys means no voice). The agent endpoints work fine.
    # User can override any of these in .env to plug in a real provider.
    if not values.get("ASR_API_KEY") and not values.get("ASR_LOCAL_BIN"):
        set_if_unset("ASR_PROVIDER", "local")
        set_if_unset("ASR_LOCAL_BIN", "/usr/bin/true")
        set_if_unset("ASR_LOCAL_TEXT_PATH", "/dev/null")
        set_if_unset("ASR_READINESS_PROBE", "0")
    if not values.get("TTS_TOKEN") and not values.get("TTS_LOCAL_BIN"):
        set_if_unset("TTS_PROVIDER", "mock")

    rendered = render_env(values, detected)

    if args.dry_run:
        print("\n--- would write " + str(ENV_PATH) + " ---")
        print(rendered)
        return 0

    if ENV_PATH.is_file() and args.reset:
        backup = ENV_PATH.parent / f".env.bak.{int(datetime.now().timestamp())}"
        backup.write_text(ENV_PATH.read_text())
        print(f"\nBacked up existing .env → {backup.name}")

    ENV_PATH.write_text(rendered)
    print(f"\nWrote {ENV_PATH} ({len(rendered.splitlines())} lines)")

    # Hint: anything the user still needs to fill manually?
    todo = []
    if "ASR_API_KEY" not in values:
        todo.append("ASR_API_KEY (for voice path)")
    if "TTS_TOKEN" not in values:
        todo.append("TTS_TOKEN (for voice path)")
    if todo:
        print("\nManual follow-up needed for these:")
        for t in todo:
            print(f"  - {t}")

    # Doubao cluster sanity-check: sales-apis uses volc_auc_meeting (a different
    # ASR cluster than bbclaw's default bigmodel). Flag it so the user knows
    # auth might still fail even though keys imported cleanly.
    db = detected.get("doubao") or {}
    if db.get("present"):
        cluster = db.get("cluster_hint", "")
        if cluster and cluster != "volc.bigasr.sauc.duration":
            print(f"\nNote: imported ASR_CLUSTER={cluster!r} differs from bbclaw default "
                  f"(volc.bigasr.sauc.duration). If you hit auth errors, your Volcengine "
                  f"app may not have access to the bigmodel cluster — open a ticket or "
                  f"override ASR_RESOURCE_ID/ASR_WS_URL manually.")

    return 0


if __name__ == "__main__":
    sys.exit(main())
