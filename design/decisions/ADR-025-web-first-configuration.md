# ADR-025: Web 优先配置 —— 把 .env 搬上管理页，分系统/AI/对话/数据四页

- **日期**: 2026-06-10
- **状态**: 已接受（v1 范围明确）
- **关联**: ADR-023（驱动管理 —— `driverstate` + `/v1/admin/drivers` 的 Web 配置先例）、ADR-021（对话编排管家 —— 项目白名单 `projectstore` 先例）、ADR-016（active_model 持久化）、ADR-010（per-device 云配置）、ADR-001（adapter 即 Agent Bus）

## 背景

Adapter 的运行配置目前**双轨制**：

- **已经 Web 化的**（env 仅一次性 seed，文件为真相，页面随时改、`/admin` 直连 loopback 网关）：
  - 项目白名单 —— `projectstore`（`projects.json`，`BBCLAW_CWD_POOL` seed）
  - 驱动 / 模型 —— `driverstate`（`driver_state.json`，`PUT /v1/admin/active_driver`）
- **还停在 `.env` 的**（`config.LoadFromEnv` 一次性读、进程级不可变）：ASR、TTS、Anthropic 代理端点、cloud relay、OpenClaw 网关、音频保存开关、各类超时/限额/会话窗口。

要新用户「装上就能用」，却还要他手写 `.env` 填 ASR/TTS 密钥——这与「默认 cloud 模式、语音在云端做」的部署现实矛盾。当前 `validateLocal()` 在 `auto` 模式下**强制** ASR/TTS 配置齐全才肯启动，等于逼着每个本地用户都配一套其实云端才用的语音管线。

**目标**：把所有「有意义的用户配置」搬到管理页，`.env` 退化成一次性 seed + 极少数 bootstrap/逃生门。页面按部署形态自适应简化（cloud 默认不显示 ASR/TTS），并拆成四页：**系统配置 / AI 配置 / 个人对话 / AI 数据文件**。

## 决策

### 1. `settingsstore` —— 复用 projectstore/driverstate 的「env-seed → 文件真相」配方

新增 `internal/settingsstore`，落 `BBCLAW_DATA_DIR/settings.json`（默认 `~/.bbclaw-adapter/settings.json`），与 `projects.json` / `driver_state.json` 同目录。约定与既有两个 store 一致：

- **原子写**（tmp + rename，0600 —— 含明文密钥，比 projectstore 的 0600 同级）。
- **损坏/缺失降级为空**，不阻塞启动。
- **`Bootstrap(path, seed)` 一次性**：无文件 → 把 env 派生的当前值写成 seed 文件；有文件 → env 被忽略（noop）。此后页面是唯一真相，`.env` 里对应项可删。
- **Apply(*config.Config) overlay**：启动时 `cfg := config.LoadFromEnv()` 先得到 env 默认/seed，再 `store.Apply(&cfg)` 用持久化值覆盖。覆盖语义：**字段「已设置」才覆盖**（空串/零值表示「未设置，沿用 env 默认」，与「显式清空」区分——见 §6 的指针/`*string` 处理）。

### 2. settings.json schema（v1）

```jsonc
{
  "version": 1,
  "topology": {
    "cloud_relay_enabled": true,    // 连云端 relay（设备语音走云）
    "local_voice_enabled": false    // 本地 LAN 语音管线（device 直连 adapter 做 ASR/TTS）
  },
  "ai": {
    "anthropic_base_url": "",       // 第三方/代理 Claude 端点，注入 claude 子进程
    "anthropic_auth_token": ""
    // 驱动/模型仍在 driverstate；项目仍在 projectstore（页面聚合展示，不在此重复存）
  },
  "voice": {                        // 仅 local_voice_enabled=true 时校验/构造
    "asr": { "provider","base_url","ws_url","app_id","api_key","resource_id","model","language","local_bin","local_args","local_text_path" },
    "tts": { "provider","token","app_id","cluster","voice","ws_url","local_bin","local_args","local_output_format" },
    "save_audio": false,
    "save_input_on_finish": true
  },
  "cloud": {                        // 仅 cloud_relay_enabled=true 时用
    "ws_url": "wss://bbclaw.daboluo.cc/ws",
    "auth_token": "",
    "home_site_id": ""
  },
  "openclaw": {
    "ws_url": "ws://127.0.0.1:18789",
    "auth_token": "",
    "node_id": "bbclaw-adapter"
  }
}
```

**范围外（仍留 `.env` 逃生门）**：监听地址 `ADAPTER_ADDR`、管理口令 `ADAPTER_AUTH_TOKEN`、`BBCLAW_DATA_DIR`（三者是 Web 服务自身起来的前置，鸡生蛋）；以及冷门调优项 —— 超时/缓冲上限/warm-pool 尺寸/会话 reuse-window/max-age（`HTTP_TIMEOUT_SECONDS`、`MAX_*`、`BBCLAW_CLAUDE_POOL_*`、`BBCLAW_SESSION_*` 等）。这些保持 env-only，页面不暴露，避免对普通用户造成噪音。

### 3. 拓扑解耦：本地 ingress ≠ 必须配语音

关键约束：**`/admin` 页由本地 HTTP ingress 提供**。所以纯 `cloud` 模式（无 ingress）就没有管理页。「默认 cloud」**不是** `ADAPTER_MODE=cloud`，而是：

- ingress 永远开着（admin 页 + 可选 LAN 语音）；
- 设备语音走 **cloud relay**（云端做 ASR/TTS），本地**不需要** ASR/TTS。

因此把「本地 ingress 启停」与「是否需要 ASR/TTS」**解耦**：

- 新增 `config.Config.LocalVoiceEnabled`（来自 settings `topology.local_voice_enabled`，env seed 可选 `BBCLAW_LOCAL_VOICE`）。
- `validateLocal()` 只在 `LocalVoiceEnabled` 时才校验 ASR/TTS、音频目录、流式限额；否则**跳过**，ingress 仅提供 admin + agent 代理。
- `buildLocalServer` 只在 `LocalVoiceEnabled` 时构造 ASR/TTS provider 和 readiness probe；否则传 nil。`handleTTSSynthesize` 已有 `s.tts == nil → 501 TTS_NOT_CONFIGURED`；ASR 路径（`/v1/stream/*`）在 nil 时同样回 `501 VOICE_NOT_CONFIGURED`（新增守卫，不再裸 panic）。

`cloud_relay_enabled` / `local_voice_enabled` 联合推导出旧的 `ADAPTER_MODE` 等价语义；`ADAPTER_MODE` env 仍作逃生门（显式 `local`/`cloud` 覆盖推导）。

### 4. Apply 模型：保存即持久化 + 重启生效（v1）

ASR/TTS/cloud-relay/openclaw 这些 provider 在 boot 时构造、深埋 pipeline，v1 **不做进程内热重载**：

- `PUT /v1/admin/settings` 写 `settings.json`，立即 200；**不影响**已在跑的 provider。
- 页面在保存后显示「⚠ 重启适配器后生效」横幅，并提供 **一键重启**：`POST /v1/admin/restart` → 先回 200，再 `syscall.Exec(self, os.Args, os.Environ())` **原地 re-exec**（单进程 daemon，重读 settings.json；在 systemd/`&` 下都等价于换新镜像）。re-exec 会中断在飞的会话——可接受，与设备切驱动语义一致。
- **保持 live、无需重启**的：驱动/模型（`driverstate`，现状）、项目白名单（`projectstore`，现状）、Anthropic 端点对**新起**的 claude 子进程下一轮即生效（claudecode 每轮新 spawn）——但因为它在 boot 时读进 `Options.Env`，v1 统一走「重启生效」以避免半生效的认知负担；横幅文案据此。

### 5. 管理页四分页

`/admin` SPA 顶栏从两 tab 扩成四页（pushState 路由）：

| 页 | 路由 | 内容 | 组件 |
|---|---|---|---|
| **系统配置** | `/admin/system` | 拓扑开关（cloud relay / 本地语音）、cloud relay（URL/token/home-site）、OpenClaw 网关、音频保存开关、状态（StatusBar）、一键重启 | `SystemPanel.vue`（新） |
| **AI 配置** | `/admin/ai` | 驱动单选（`DriversPanel`，现状 live）、Anthropic 端点+token、项目白名单（`ProjectsPanel`，现状）、**ASR/TTS（仅本地语音开启时显示）** | `AiPanel.vue`（新，聚合） |
| **个人对话** | `/admin/conversations` | 对话记录（只读，现状） | `Conversation.vue` |
| **AI 数据文件** | `/admin/files` | workspace `CLAUDE.md` + `MEMORY/*.md` 预览（只读，现状） | `FilesPanel.vue` |

页面**按拓扑自适应**：`local_voice_enabled=false` 时 AI 页隐藏整块 ASR/TTS 表单；`cloud_relay_enabled=false` 时系统页折叠 cloud relay 段。视觉沿用点阵设计语言（`design/UI_DESIGN_LANGUAGE.md`）。

### 6. HTTP 契约

全部 loopback-gated（`adminLocalOnly`），与既有 admin 路由同源：

- `GET /v1/admin/settings` → `{ok,data:{settings:{...}, restart_required:bool}}`。**明文返回密钥**（接口仅 loopback，按最小实现选明文读写）。`restart_required` 为「上次写 settings 后是否尚未重启」——进程内存一个 dirty 标志，re-exec 后自然清零。
- `PUT /v1/admin/settings` body 为 §2 schema（可部分字段 patch；后端按指针字段做「在场才改」合并），校验后落盘，置 dirty。校验失败回 `400 INVALID_SETTINGS` + detail。
- `POST /v1/admin/restart` → 200 后 re-exec。

## 跨组件约束（务必同步）

| 改动 | 同步检查 |
|---|---|
| settings 新增「会进 cloud relay/路由」的字段（如 cloud auth、openclaw） | `internal/homeadapter` 是平行实现，relay 侧若读同一项需一并接 settingsstore.Apply |
| `validateLocal` 放宽 ASR/TTS | `homeadapter` 的 `buildCloudRelay` 不受影响（云 relay 不构造本地 ASR/TTS），但 `main.go` 两条启动路径都要读 `LocalVoiceEnabled` |
| Anthropic 端点搬入 settings | `butlerMCPEnv`（mcp-server 子进程注入）与 claudecode `Options.Env` 两处都从 overlay 后的 `cfg` 取，保持一致；`LoadButlerEnv` 也走 settingsstore |
| `.env` 字段语义变 seed | README / `.env.example` / `docs/butler.md` 文案改为「首启 seed，之后去 /admin 改」 |

## 范围外（v1 已知限制）

- **不做热重载**：transport/voice provider 改动靠 re-exec 重启生效（§4）。
- **密钥明文**：GET 直接回真实值、页面可见可编。loopback-only 下可接受；将来若 admin 开放到 LAN 再加掩码（set-only）。
- **不走 WS 广播**：多 admin 客户端同开时靠 refetch，不保证即时一致（与 ADR-023 同限制）。
- **bootstrap 三项 + 冷门调优项仍 env-only**（§2 范围外），不进页面。
- **`settings` 无云 relay kind**：配置页是 localhost 直连；固件/云不读 settings。
