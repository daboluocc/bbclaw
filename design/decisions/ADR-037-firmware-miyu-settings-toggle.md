# ADR-037: 固件设置菜单密语(miyu)开关

- **日期**: 2026-06-25
- **状态**: 已接受（owner 确认：重启生效 / 灰度走现有云端 OTA 渠道 / 固件改完由 owner 发布）。待 `make build` 通过。
- **关联**:
  - 音量行（`MAIN_ROW_VOLUME` + `bb_device_config_set_volume_pct`）与 TTS 开关行（`MAIN_ROW_TTS`）——本开关照抄它们的范式。
  - adapter_v2 设备控制（管理页 / `device set-miyu` CLI → 云端 config → 固件）——本 ADR 补齐**固件设备端**这一处，让密语和音量一样「固件菜单 + adapter 页 + CLI + 管家语音」四处可控。
  - `bb_state.c`（开机据 `miyu_enabled` 决定是否进 LOCKED）、`bb_radio_app.c`（云端关密语时 LOCKED→CHAT 运行时切换）。

## 背景

**密语(miyu) = 锁屏语音解锁**：开启后（仅 cloud_saas）设备开机进 `BB_PAGE_LOCKED`，必须语音口令验证才解锁（`bb_radio_app.c:388` `passphrase_unlock_enabled()` = cloud_saas && `miyu_enabled`）。

现状（勘查）：`miyu_enabled` 字段在 `bb_device_config.h:19`，由云端 `config.update`/welcome 下发、存 NVS；但**固件设置菜单没有密语开关**——音量有 `MAIN_ROW_VOLUME` 行、TTS 有开关行，密语两者都没有，只能云端/CLI 改，且**没有 `bb_device_config_set_miyu()` setter**。诉求：在固件设置菜单加密语开关，和音量一样设备端可调。

## 决策

在设置菜单加一个**布尔开关行 `MAIN_ROW_MIYU`**（"Miyu: on/off"），照抄 TTS 开关范式（在位切换、不进子页），持久化照抄音量的 commit 路径：

1. **`bb_device_config_set_miyu(int enabled)`**（`bb_device_config.c/.h` 新增）——clamp 0/1、`version++`、`persist_config()`，与 `bb_device_config_set_volume_pct` 同构（本地发起的改动 bump version，与云端 `apply_update` 的版本门协调一致）。
2. **`MAIN_ROW_MIYU`** 入 `main_row_t` 枚举（VOLUME/TTS 之后）；`main_visible_rows()` **仅 cloud_saas 显示**（密语只在 cloud_saas 有意义，与 ADAPTER/SESSIONS 行一致）。
3. **渲染** "Miyu: on/off"（照 TTS 的 "Voice: on/off"）；**点击**在 `LEVEL_MAIN` 在位翻转 `s_st.miyu_enabled` → `spawn_persist_int(COMMIT_KIND_MIYU, v)`（新 commit kind，commit_task 走内部 RAM 栈做 NVS 写，照 VOLUME/TTS）。
4. **进入设置时**从 `s_config.miyu_enabled` 读进 `s_st.miyu_enabled`（照音量 `s_st.volume_pct = bb_device_config_get()->volume_pct`）。

### 生效时机：重启生效（owner 确认）

密语只在**开机时**决定锁不锁屏（`bb_state.c`），所以本地切换天然「下次重启生效」——正好和 owner「变更要重启」一致，也**避免「开密语」时当场把自己锁在设置页外**（设置页从 CHAT 进，开密语后仍是 CHAT，下次开机才锁）。故本期**不做运行时即时 lock-state 切换**（云端关密语的运行时 LOCKED→CHAT 路径 `bb_radio_app.c:3388` 不变，是另一条云驱动路径）。

### 灰度发布（owner 确认）

固件侧**无 canary 机制**，OTA 是 tag→构建→所有设备一次性拉取。灰度是**云端 OTA channel(stable/canary)** 的事，固件侧不改。本固件变更经现有云端 OTA 渠道灰度发布、设备重启拉取后生效（owner 触发发布，本 ADR + 代码不打 tag）。

## 不做

- 运行时即时生效（见上，重启生效更安全、且符合 miyu 语义）。
- 设置页打开时的云端密语变化 live-refresh（密语经 config.update 非心跳路径，且菜单驻留短、密语变化罕见；进入时读 `s_config` 即可。需要再补，照 `bb_ui_settings_notify_volume_pct`）。

## 跨组件

无新协议。密语数据模型（`miyu_enabled` + cloud config.update/welcome + NVS）已存在；本 ADR 只加固件端**本地写**入口（菜单开关 + setter），与云端下发共用同一 `s_config.miyu_enabled` 字段。

## 后果

- 密语和音量对齐：固件菜单 / adapter 管理页 / `device set-miyu` CLI / 管家语音 四处皆可控。
- 设备端用户能自己开/关锁屏语音解锁，不必依赖云端或管家。
- 重启生效（符合 miyu 锁屏语义 + owner 预期），无把用户锁在设置外的风险。
- 经现有云端 OTA 渠道灰度发布；本仓不打 tag，由 owner 触发。
