# ADR-039: 密语(miyu)离线死锁修复 — 断网不进锁屏 + 锁屏期间持续离线自动解锁

- **日期**: 2026-06-26
- **状态**: 已接受（owner 确认：离线 = 自动解锁到 CHAT，安全让位于「别被锁死」）。待 `make build` 通过 + 真机验证。
- **关联**:
  - [[ADR-037]] 固件设置菜单密语开关、[[ADR-038]] 密语解锁失败显示识别文本——本 ADR 是同一密语锁屏特性的**离线鲁棒性**补丁。
  - `bb_radio_app.c`：`passphrase_unlock_enabled()`（cloud_saas && `miyu_enabled`）、闲置升级 `CHAT→LOCKED`（`stream_task` 主循环）、运行时云端关密语 `LOCKED→CHAT` 的 reconcile。
  - `s_transport_health_ok`：主循环维护的「云端/adapter 可达」健康位（boot 健康检查、心跳失败 streak、WiFi 掉线/重连事件共同驱动）。

## 背景

**密语锁屏的解锁只有一条路**：把语音发到云端 `voice.verify` 校验（`bb_adapter_voice_verify_pcm16`）。
而是否进/留在锁屏只看 `bb_transport_is_cloud_saas() && miyu_enabled`，**完全不看网络是否可达**。
于是构成「离线死锁」：

1. **网络报错**（典型 "CLOUD ERR" = WiFi 连着但云端不可达）后设备照常闲置。
2. CHAT 闲置 `BBCLAW_STANDBY_LOCK_TIMEOUT_MS`(120s) → `passphrase_unlock_enabled()` 仍为真 → `set_radio_app_state(LOCKED)`，**息屏进密语模式**。
3. 想解锁 → PTT 录音 → 发云端校验 → 云端不可达 → **一直「解锁失败」** → 永久锁死。

同样的坑还有 **开机路径**：开机若 miyu 开着就直接进 `LOCKED`（`bb_radio_app_start`），此时若没网照样解不开；
以及 **锁屏后才断网**：已经在 LOCKED，网一断就出不来。

> 注：屏幕本身没断电，「息屏」= 页面切到 STANDBY→LOCKED 静态锁屏页。根因是「不该在断网时锁屏 / 不该在断网时困在锁屏」。

## 决策

**离线优先「别被锁死」，安全（密语防护）让位**（owner 拍板）。两处协同改动，均在 `bb_radio_app.c` 主循环（`stream_task`）：

1. **断网不自动锁屏**：闲置升级 `CHAT→LOCKED` 增加网络健康门槛——只有 `passphrase_unlock_enabled() && s_transport_health_ok` 才进 LOCKED。
   `s_transport_health_ok` 为假（断网/云端不可达/WiFi 掉线/重连中）时**停在 CHAT/STANDBY 不锁**，并照常刷新闲置计时（网络恢复后 ≤120s 内恢复正常锁屏，安全姿态自动回归）。
   这直接消除「网络报错后闲置→锁死」最常见路径，也避免「锁→立刻离线自动解锁→再锁」的抖动。

2. **锁屏期间持续离线 → 自动解锁到 CHAT**：救「开机即锁但没网」「锁屏后才断网」两种场景。
   主循环跟踪「连续处于 `LOCKED && !s_transport_health_ok`」的起始时刻，持续超过
   `BBCLAW_LOCKED_OFFLINE_AUTO_UNLOCK_MS`(默认 15s) 即 `set_radio_app_state(CHAT)`。
   照抄已有「运行时云端关密语 → LOCKED→CHAT」reconcile 的范式（同一处、同一手法）。
   - 15s 宽限期**避开瞬时抖动**（WiFi 短暂重连一般 5–10s 内恢复，恢复即把健康位置 1、计时清零，不会误解锁）。
   - 主循环在 `wait_for_transport_health()` 之后才创建，故循环首跑时 `s_transport_health_ok` 已反映开机连通性；开机离线 = 健康位从一开始就是 0 → 锁屏约 15s 后自动解锁，**无需额外「boot settled」标志**。

### 安全权衡（owner 确认）

自动解锁意味着**拔网/断网即可绕过密语**，密语对「他人误用/防蹭用」的防护被削弱。
owner 明确选择此取舍：本设备威胁模型是「别把主人自己锁在门外」，而非防范有意攻击者；
「设备是我自己的，别锁死我」优先级高于密语强度。

## 不做

- **不做离线按键旁路解锁**（如锁屏页长按组合键关密语）：自动解锁已覆盖诉求，按键旁路是更重实现，本期不做。
- **不改 `passphrase_unlock_enabled()` 语义**：它仍只表达「该特性是否启用」（cloud_saas && miyu）；网络健康是**调用点**的独立门槛，不混入该函数（开机锁屏判定 `bb_radio_app_start:3554` 仍按特性启用即锁，离线由循环里的自动解锁兜底）。
- **不动 WiFi 配网/重连页路径**：那两个分支自有 apconfig / RECONNECTING 视图覆盖锁屏，且会 `continue` 跳过闲置块；用户实际命中的是 WiFi 连着但云端不可达（CLOUD ERR），该路径正常落到闲置块，已被覆盖。

## 跨组件

无新协议、无 envelope/HTTP 契约变化——纯固件端 UI 状态机的离线兜底。云端/adapter 不需同步。
（参见 [[project_release_ota_workflow]]：固件改动经 OTA 发布；本 ADR + 代码先真机验证再发版。）
