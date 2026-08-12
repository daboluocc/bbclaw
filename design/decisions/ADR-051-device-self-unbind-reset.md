# ADR-051: 设备端自助解绑与重置——菜单一键回到出厂待配对态

- **日期**: 2026-08-12
- **状态**: 已接受（随本 ADR 一并落地实现）
- **组件**: firmware + cloud（新增一条 device-facing HTTP 契约，需**云端先部署、固件后发 OTA**）
- **关联**:
  - `ADR-050`（WiFi 重新配网提案）——正交：本 ADR 的重置会整体清掉 WiFi 凭据走 boot 配网路径，不依赖 ADR-050 的运行期阶梯
  - Cloud 现有 portal 解绑：`DELETE /v1/bbclaws/release`（`portal_api.go handleBBClawRelease` → `account.Store.ReleaseDevice`）
  - 固件配对流程：`bb_cloud_client.c bb_cloud_pair_request`（POST `/v1/pairings/request`，状态由云端 `IsClaimed` 决定）

## 1. 背景

### 1.1 现状

- **设备本地不存任何绑定状态。** `device_id` 由芯片 MAC 派生（`bb_identity.c`，`BBClaw-XXXXXX`），跨 OTA 稳定；配对/认领关系全部在云端 account store（`Device.OwnerUserID` + `Binding` + `Allocation`）。
- **解绑目前只有 portal 一条路**：用户登录 web，在设备设置页点「解绑设备」→ `DELETE /v1/bbclaws/release`（session auth）。设备侧没有任何入口。
- **固件没有恢复出厂能力**：全仓无 `nvs_flash_erase` / factory-reset 入口。WiFi 凭据、设备配置、会话缓存等散在默认 NVS 分区各 namespace 里，只有逐 key 的删除。

### 1.2 缺口

设备转手 / 换账号 / 退货 / 展示机清场时，需要**在设备上**完成「解绑账号 + 清空本地数据 + 回到全新开机状态」，不依赖用户还能登录原账号的 portal。这是消费级设备的常规能力（手机/音箱的 factory reset 均可脱离原账号执行，语义 = 物理持有即可重置）。

## 2. 目标 / 非目标

**目标**
1. 设备设置菜单新增「Reset Device」：确认后云端解除 claim + 本地 NVS 全擦 + 重启。
2. 重启后设备回到全新状态：无 WiFi → SoftAP 配网门户；配网后 `/v1/pairings/request` 返回 `claim_required` → 屏幕重新出 6 位配对码（含 TTS 播报），任何账号可重新认领。
3. 失败安全：云端解绑失败时**不擦本地**（见 §3.3 陷阱），行内提示可重试。

**非目标**
- 不做 portal 解绑时向设备推送「你已被解绑」通知（现有 portal 解绑本来就是"下次重连才生效"，不在本 ADR 扩展）。
- 不做选择性重置（只清 WiFi / 只清会话）。一把梭全擦。
- 不动 otadata / app 分区——重置不改变已刷入的固件版本。

## 3. 决策

### 3.1 Cloud：新增 device-facing `POST /v1/pairings/release`

```
POST /v1/pairings/release
{"deviceId": "BBClaw-XXXXXX"}
→ 200 {"ok":true,"data":{"released":true}}
```

- 挂在设备 HTTP 面路由组（`server.go`，紧邻 `POST /v1/pairings/request`），`withHTTPAuth` 网关（与 request 一致）。
- Handler 调新的 store 方法 `ReleaseDeviceByDeviceID(deviceID)`：与 `ReleaseDevice(userID, deviceID)` 同一清理逻辑（清 `OwnerUserID`/`DisplayName`/`ActiveHomeSiteID`，级联删 `Binding` + `Allocation`），但**不做 owner 校验**——调用方就是设备自己。
- **幂等**：设备不存在或本就未认领 → 也返回 `ok:true, released:false`。固件只看 2xx + ok，重复重置不报错。
- **信任模型说明**：设备 HTTP 面从来就是 deviceId 即身份、无设备侧密钥（`/v1/pairings/request`、`/v1/devices/info` 同款）。本端点不降低现有安全水位；语义上等价于「物理持有设备的人可以重置它」，与消费级设备惯例一致。知道 deviceId 的远程攻击者能造成的最坏结果是解绑骚扰（DoS），与现有「伪造 deviceId 抢先认领」属同一暴露面，治本要靠未来的设备密钥体系，不在本 ADR 范围。

**为什么走 HTTP 不走 WS envelope**：设备 WS 的 authBypass 依赖「已配对/已绑定」（`ResolveActiveHomeSite`），处于 `binding_required` 等半绑定状态的设备可能根本建立不起 WS；而重置恰恰要覆盖这类残局。HTTP 面无此前置，且固件 `bb_cloud_client` 现成。

### 3.2 Firmware：Settings 新增「Reset Device」行

五处联动（`bb_ui_settings.c` 惯例）：`main_row_t` 加 `MAIN_ROW_RESET`；`main_visible_rows()` 排在 `MAIN_ROW_SYSINFO` 前、**所有模式常显**（local_home 也需要清 WiFi/本地态）；`render_main()` 行文案；`handle_click()` 行为；行状态字段。

- **确认交互**：沿用双击确认惯用法（`recorder_arm_ms`/`miyu_arm_ms` 同款 5s 窗口）。首击武装，行文案变 `Reset · tap again to UNBIND+WIPE`；窗口内再击执行。
- **执行序列**（cloud_saas）：
  1. PSRAM 栈任务（16KB，同 `ota_check_task`——TLS 吃栈）调 `bb_cloud_release_pairing()`；行文案 `Reset · resetting…`，期间去重防重入（`reset_pending`）。
  2. 成功 → spawn **内部栈**任务（plain `xTaskCreate`，同 `config_persist_task` 的 cache-safe 理由：NVS 擦除期间 flash cache 关闭，PSRAM 栈不可访问）：`nvs_flash_deinit()` → `nvs_flash_erase()` → `esp_restart()`。
  3. 失败（非 2xx / 网络错）→ 行文案 `Reset · cloud failed, retry`，**本地不擦**。
- **local_home**：跳过云端调用，直接进擦除任务。
- **擦除范围** = 默认 `nvs` 分区整体（WiFi 凭据槽、`bbclaw` ns 的 config/session/chat cache/theme、`ota` ns、`bbpwr` ns 一锅端）。不碰 otadata 分区，OTA 槽与当前运行固件不受影响。

### 3.3 为什么云端失败必须挡住本地擦除

device_id 由 MAC 派生、跨重置稳定。若云端 claim 未解除就擦本地：设备重启配完网后 `/v1/pairings/request` 直接返回 `approved`——**看起来重置了，实际还绑在原账号上**，比不重置更糟（用户以为已脱敏转手）。所以顺序必须是「云端先解绑成功，才允许擦本地」；反过来云端解绑成功后本地擦除即使失败/断电，重启后设备也只是回到 `claim_required`，无残留绑定，方向安全。

### 3.4 发布顺序（跨组件契约）

1. **云端先部署**（bbclaw-reference，服务端热发，无 tag）。旧固件不调新端点，零影响。
2. **固件后发 OTA**（`v*` tag 走 release.yml）。若固件先发而云端未部署：`/v1/pairings/release` 404 → 固件按失败处理、不擦本地，行为安全退化为「重置暂不可用」。

## 4. 重启后的用户旅程（验收标准）

1. 菜单 Reset → 双击确认 → 屏幕短暂 `resetting…` → 设备重启。
2. 无 WiFi 凭据 → 进 SoftAP 配网门户（`BBClaw-Setup-<MAC>`）。
3. 配网完成 → 配对轮询返回 `claim_required` → 屏幕出新 6 位配对码 + TTS 播报。
4. 原账号 portal 设备列表中该设备已消失（owner 已清）；任意账号可用新码认领。
5. 云端解绑失败场景：行内报错、可重试，本地数据原样、设备仍正常可用。
