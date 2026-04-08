---
name: bbclaw-adapter-install
description: BBClaw 局域网 bbclaw-adapter 外部安装 Skill：按平台下载 Release 二进制、生成 .env、启动与健康检查；供复制到各 Agent「技能」目录后由 AI 代用户一键完成主机侧安装。
---

# BBClaw Adapter 外部安装 Skill（参考）

本文件是 **项目内文档**，格式对齐外部仓库的 Skill 约定（YAML frontmatter + 分步指令），便于你复制到 **Cursor / Codex / 其他 Agent** 的 skills 目录，或整段交给 AI 作为「帮用户装 Adapter」的操作说明。

**格式参考**：[Star-Office-UI `SKILL.md`](https://github.com/ringhyacinth/Star-Office-UI/blob/master/SKILL.md)

**人类可读手册**（烧录固件、常驻运行等）：仓库 [docs/getting-started/user_guide.md](../getting-started/user_guide.md) 与 [GitHub Wiki](https://github.com/daboluocc/bbclaw/wiki)。

---

## 0. 一句话告诉用户这是什么

你可以先说：

> `bbclaw-adapter` 是跑在你家电脑/NAS 上的小服务：BBClaw 设备通过 HTTP 把语音流发给它，它做 ASR 并通过 WebSocket 连到你的 OpenClaw Gateway。Release 里 **每个平台只有一个二进制**，用环境变量配置，**没有** `-c xxx.yaml`。

---

## 1. 你要达成的目标（AI 自检）

- 在用户机器上创建目录（例如 `~/bbclaw-adapter`），下载 **正确架构** 的 `bbclaw-adapter`。
- 写好 **`.env`**（至少：`ADAPTER_AUTH_TOKEN`、`OPENCLAW_WS_URL`、与 ASR 相关的变量）。
- `chmod +x`（Unix）后 **`set -a; source .env; set +a`** 再启动进程。
- 用 `curl` 打 **`/healthz`** 确认带 Bearer 时返回正常。
- 若用户要 **公网 Home Adapter**（`ADAPTER_MODE=cloud`），不要在本 Skill 里硬编全套 Cloud 变量；请引导用户阅读 Wiki [公网版 Home Adapter 部署](https://github.com/daboluocc/bbclaw/wiki/%E5%85%AC%E7%BD%91%E7%89%88-Home-Adapter-%E9%83%A8%E7%BD%B2) 或使用 `.env.home`。

---

## 2. 检测平台并下载（优先执行）

在 **用户的目标机器** 上执行（把 `~/bbclaw-adapter` 换成用户认可的目录）：

```bash
mkdir -p ~/bbclaw-adapter
cd ~/bbclaw-adapter
```

按 `uname -s` + `uname -m`（或 Windows 环境）选择 **一条** `curl`（Release 始终指向最新版）：

| 环境 | 命令 |
|------|------|
| macOS Apple Silicon | `curl -fL -o bbclaw-adapter https://github.com/daboluocc/bbclaw/releases/latest/download/bbclaw-adapter-darwin-arm64` |
| macOS Intel | `curl -fL -o bbclaw-adapter https://github.com/daboluocc/bbclaw/releases/latest/download/bbclaw-adapter-darwin-amd64` |
| Linux x86_64 | `curl -fL -o bbclaw-adapter https://github.com/daboluocc/bbclaw/releases/latest/download/bbclaw-adapter-linux-amd64` |
| Linux ARM64 | `curl -fL -o bbclaw-adapter https://github.com/daboluocc/bbclaw/releases/latest/download/bbclaw-adapter-linux-arm64` |
| Windows | 建议用户浏览器下载 `bbclaw-adapter-windows-amd64.exe`，或用 PowerShell `Invoke-WebRequest` 下载同 URL；运行时配置环境变量方式不同，见下文说明 |

Unix：

```bash
chmod +x bbclaw-adapter
```

---

## 3. 生成 `.env`（局域网 / `ADAPTER_MODE=local`）

在 `~/bbclaw-adapter/.env` 写入骨架，**把占位符换成用户真实值**：

```bash
ADAPTER_MODE=local
ADAPTER_ADDR=:18080
ADAPTER_AUTH_TOKEN=请与用户约定并与固件侧 Bearer 一致

OPENCLAW_WS_URL=ws://127.0.0.1:18789
OPENCLAW_AUTH_TOKEN=
OPENCLAW_NODE_ID=bbclaw-adapter

# ASR：根据用户环境三选一补齐（未配置完整时进程会报错退出）
# A) 本地命令行 ASR
ASR_PROVIDER=local
ASR_LOCAL_BIN=
# ASR_LOCAL_ARGS=...  # 需 {wav} 占位，见固件仓库 firmware/docs/bbclaw_adapter_integration.md 与 adapter .env.example

# B) OpenAI 兼容 HTTP
# ASR_PROVIDER=openai_compatible
# ASR_BASE_URL=
# ASR_API_KEY=
# ASR_MODEL=

# C) 火山 doubao_native
# ASR_PROVIDER=doubao_native
# ASR_WS_URL=...
# ASR_APP_ID=...
# ASR_API_KEY=...
```

**你必须向用户确认的最少信息**：

1. OpenClaw Gateway 的 WebSocket 地址（默认常为 `ws://127.0.0.1:18789`）。
2. 与固件一致的 **`ADAPTER_AUTH_TOKEN`**（若用户尚未在固件侧配置，你应生成强随机串并提醒用户同步到设备/配置流程）。
3. ASR 方案：本地可执行文件路径，或兼容服务 URL + Key，或火山参数。

更多变量（TTS、`MAX_CONCURRENT_STREAMS` 等）见固件仓库内 **adapter** 的 `.env.example`（若本地无 monorepo，可打开 [bbclaw 源码树中 firmware/docs](https://github.com/daboluocc/bbclaw/tree/main/firmware/docs) 链接的对接说明）。

---

## 4. 启动与健康检查

```bash
cd ~/bbclaw-adapter
set -a && source .env && set +a
./bbclaw-adapter
```

日志中应出现类似 **`starting bbclaw-adapter mode=local addr=...`**。

**另开终端**（环境变量与 token 一致时）：

```bash
curl -sS -H "Authorization: Bearer 与.env中ADAPTER_AUTH_TOKEN相同" "http://127.0.0.1:18080/healthz"
```

---

## 5. 安装成功后提醒用户的三件事

1. **固件侧**：`local_home` 模式下 menuconfig 里的 **Local Adapter Base URL** 必须是 `http://<这台机器局域网IP>:18080`（端口与 `ADAPTER_ADDR` 一致）。
2. **安全**：不要把 `.env` 提交到 git；token 与生产环境一致时勿截图发公开渠道。
3. **常驻**：需要开机自启时，引导用户按 [使用手册](https://github.com/daboluocc/bbclaw/wiki) 中的 launchd / systemd 示例，用 **`EnvironmentFile=`** 或 wrapper 脚本 `source .env` 再 `exec` 二进制。

---

## 6. Windows 补充

- 二进制名为 `bbclaw-adapter-windows-amd64.exe`；可用「系统环境变量」或 `set` 当前会话设置与 `.env` 同等变量。
- 健康检查示例：`curl -H "Authorization: Bearer ..." http://127.0.0.1:18080/healthz`（需已安装 curl 或用等价工具）。

---

## 7. 给你的提示（AI）

- **尽量自己执行**下载、写 `.env`、启动、curl 验证；用户只补充 OpenClaw 地址、token、ASR 三类信息即可。
- **不要**假设存在 `-c adapter.yaml`；当前主线配置只有 **环境变量**。
- **不要**在文档或对话中复述用户完整 token；日志回显时注意脱敏。
- 若 `go test` / 本机开发环境与用户无关，**不要**要求用户克隆整个 bbclaw 再 `make go-build`，除非用户明确要源码编译。

---

## 8. 作为「外部 Skill」安装到 Agent 的方式（人类操作）

1. 将本文件复制到你的技能目录，例如 Cursor 的 `.cursor/skills/bbclaw-adapter-install/SKILL.md`（目录名可自定）。
2. 保留顶部 `--- name / description ---` 块，便于工具识别。
3. 若你的工具要求 `SKILL.md` 固定文件名，可把本文件 **重命名为** `SKILL.md` 再放入子目录。

仓库内保留本路径是为了与文档站/版本控制同步；**以外部工具为准**时可只复制内容，不必提交到你的私有 skills 仓库以外的位置。
