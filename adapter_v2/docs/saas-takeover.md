# Phase 4（待办）— SaaS 下手机端「完美接管」终端

> 状态：**设计待办**。等 adapter_v2 Phase 1（LAN 直连终端 + 重连）和 Phase 2（设备/语音线）完成后再实现。
> 这一阶段主要动 **Cloud（`bbclaw-reference/cloud`）**，adapter_v2 侧基本不改（termchan 协议已是 relay-friendly 的小 JSON 帧）。

## 目标

用户人在外网，掏出手机就能**接管**家里 HomeAdapter 上正在跑的 Coding Agent 终端：看到当前屏、抢过键盘输入、断网息屏不丢会话——和在 LAN 里用 web 客户端体验一致。

## 关键前提：难点已被 adapter_v2 的 session 设计解决

「完美接管」的四个能力，Phase 1 的 `session` + `termchan` 已经天然提供，SaaS 只是把这些帧**经 Cloud 路由**到对的 HomeAdapter：

| 接管能力 | 实现 | 来源 |
|---|---|---|
| 看到正在跑的实时屏 | attach → 收 snapshot + scrollback 重放 | ✅ Phase 1 已实现 |
| 抢过输入权（真正"接管"） | 单写者策略：最新连接独占 stdin，旧连接降级只读旁观（`termchan.writerSet`，generation counter） | ✅ Phase 1 已实现 |
| 断网/息屏不丢会话 | session 独立存活，Detached 5min 才回收；回来重 attach 补快照 | ✅ Phase 1 已实现 |
| 手机与语音设备看同一会话 | 同一 session 多 client 消费 | ✅ "单 PTY 双视图"设计 |

## 数据流（复用 BBClaw 既有 Cloud SaaS 出站隧道）

```
 手机浏览器/PWA            Cloud (bbclaw.daboluo.cc)          HomeAdapter (adapter_v2)
   xterm.js ──wss──▶ 鉴权/多租户路由/帧 mux ──既有出站隧道──▶ termchan /ws ─▶ session ─▶ PTY
           ◀──── 终端帧 (output/snapshot/reconnected) ────────────────────◀
```

- **NAT 穿透**：沿用现有模式——HomeAdapter 主动拨出 `CLOUD_WS_URL=wss://bbclaw.daboluo.cc/ws`（`CLOUD_AUTH_TOKEN`），家里**不开任何入站端口**。
- **多租户路由**：手机登录 Cloud 账号 → Cloud 查到该用户名下的 HomeAdapter → 定向到它。复用现有设备注册 + 密语（蜜语）层。

## 要新写的（都在 Cloud 侧）

1. **帧 mux（主要工作）**：现有出站 relay WS 已承载设备音频，需在其上加一种**终端流类型**，按 `streamType=terminal` + `sessionId` 给帧打标，让一条隧道同时承载音频与终端。需要一条 ADR 固化这个 mux 协议。
2. **鉴权粒度**：终端 = 远程完整 shell 权限，比语音危险得多。每个终端 session 独立 token + 账号鉴权；建议复用设备已有的**密语锁屏**作为接管前的二次确认。
3. **Cloud 托管 web 客户端**：SaaS 下由 Cloud serve（或做成 PWA）adapter_v2 的 web 客户端，`/ws` 经 relay 反代到 HomeAdapter。

## 客户端形态（从轻到重，建议止步 PWA）

- **MVP**：响应式 web，Cloud 托管，手机浏览器打开（与 dinotty 同路子，零 app 成本）。
- **进阶（建议终点）**：PWA——可安装、全屏、接推送（配合现有通知系统），覆盖 95% 诉求。
- **原生 app**：仅当需要后台常驻 / 深度系统推送时才做，大投入，不建议早做。

## 开源参考（dinotty）能给什么

- ✅ 可借鉴：web 终端客户端、重连补快照机制。
- ❌ 没有：**原生移动 app**（`src-tauri` bundle targets 仅 `app/dmg/appimage/deb`，纯桌面，无 android/ios）；**SaaS 中继**（README 自述"中继服务：计划中"，未实现）。

→ 结论：app 层与中继层 dinotty 都没有，但 **BBClaw 已有现成 cloud relay 架构**，这一阶段是"在既有隧道上加一条终端流"，而非从零造中继。

## 验收（将来）

1. 手机外网浏览器登录 Cloud → 选会话 → 看到家里 agent 当前屏。
2. 手机输入抢过键盘，LAN 端 web/设备降为只读旁观。
3. 手机切后台/断网 60s 再回 → 会话还在、屏幕还原。
4. 密语二次确认通过后才放行终端接管。
