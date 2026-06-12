# ADR-027: 设备端切换 Home Adapter（切机器）— cloud 组装的设备态选择器

- **日期**: 2026-06-12
- **状态**: 提议（待评审）
- **关联**: ADR-004（cloud_saas Agent Bus 代理）、ADR-010（per-device driver 云配置）、ADR-016（设备端 driver/model 选择）、ADR-019（server-driven 菜单协议）、ADR-025（web 优先配置）

## 背景

cloud 端（`bbclaw-reference`）已经有完整的**多 home adapter 绑定模型**：

- `account.Store` 里一个设备可关联多条 `Binding`，但同一时刻只有一条 `IsActive=true`（`cloud/internal/account/store.go:125-130`）。
- 切换靠 `ActivateBinding(userID, deviceId, homeSiteId)`（在已有绑定间切 active）与 `CreateBinding`（新绑定，自动替换旧的）。
- 路由时 `resolveDeviceHomeSite()` 动态查 active binding，relay 到对应 adapter（`cloud/internal/router/hub.go:226-308`）。

但**切机器的入口只在 Web 控制台**（`web/src/pages/device-tabs/AdapterTab.jsx`：下拉选已认领 adapter、绑定/解绑）。

**需求**：每台 BBClaw 设备能在**本地 Settings** 里直接选"现在连哪台 home adapter（机器）"，不用打开 Web 后台。这样每个设备可以独立切到不同机器进行管理（白天用家里 Mac、出门切公司那台）。**范围仅 cloud_saas 模式**（local_home 设备直连固定 adapter IP，无"多机器"概念，本 ADR 不涉及）。

### 与 ADR-010 / ADR-025 的张力（必须正面处理）

ADR-010 的核心论点是"切 driver 是**管理员动作**，应在管理面（web portal）做，不在设备做"；ADR-025 进一步把配置统一"web 优先"。表面上"在设备上切机器"与这个方向冲突。

化解：区分**授权层**与**使用层**。

- **授权层（管理员动作，留在 Web）**：claim 一台 adapter、把某台 adapter 绑定给某设备。这决定"这台设备**被允许**连哪些机器"。本 ADR 不动。
- **使用层（用户动作，可下放设备）**：在**已授权的机器集合内**选"现在用哪一台"。这是 per-device、高频、纯使用态的选择，与 ADR-016 把 driver/model 切换放到设备 Settings 的理由同构。

边界由此清晰：**设备只能在"已绑定到本设备"的 adapter 之间切 active，不能新增绑定**。绑定/claim 仍是 Web 管理员动作。这恰好维持了 ADR-010 的"管理员授权、用户使用"分层，而非推翻它。

## 决策

### 1. 数据真相源在 cloud，菜单由 cloud 直接组装（区别于 ADR-019 现有菜单）

ADR-019 的 sessions/cwd/drivers/models 数据都在 **home adapter 本地**，cloud 仅 relay 透传。但"该设备有哪些可切机器"是 **cloud-only 数据**（account bindings + `ListUserHomeAdapters`），home adapter 不掌握。

因此本菜单**在 cloud 终结组装，不 relay 到 home adapter**。这是它与所有现有 server-driven 菜单的本质差异，也是为什么不能简单照搬 ADR-019 现成 relay 路径。

### 2. 设备态 WS 控制协议（新增两个 kind，cloud 终结）

设备在 cloud_saas 下与 cloud 之间已有**已鉴权的 WS 控制通道**（握手时 `device_id` 已知，已绑定设备免 token）。新增两个设备发起、cloud 终结的控制消息：

| kind | 方向 | cloud 行为 | 回包 |
|---|---|---|---|
| `sites.list` | device → cloud | 反查 device 的 owner + 已绑定 adapter 列表 | `{sites:[{homeSiteId,label,online,active}]}` |
| `sites.activate` | device → cloud | 校验目标已绑定本设备且同 owner → `ActivateBinding` | `{result:"ok",activeHomeSiteId}` 或 `{error:{code,detail}}` |

复用现有信封（`router.Envelope`：`type` + `kind` + `messageId` + `payload`）。精确契约（device↔cloud 逐字一致，firmware/cloud 两侧 issue 已锁定同一份）：

```json
// 请求 device→cloud
{"type":"request","kind":"sites.list","messageId":"<seq>","deviceId":"<id>"}
{"type":"request","kind":"sites.activate","messageId":"<seq>","deviceId":"<id>","payload":{"homeSiteId":"<id>"}}
// 回包 cloud→device（带回请求的 messageId）
{"type":"response","kind":"sites.list","messageId":"<seq>","payload":{"sites":[{"homeSiteId":"hs-1","label":"家里 Mac","online":true,"active":true}]}}
{"type":"response","kind":"sites.activate","messageId":"<seq>","payload":{"result":"ok","activeHomeSiteId":"hs-2"}}
{"type":"response","kind":"sites.activate","messageId":"<seq>","error":{"code":"NOT_BOUND","detail":"..."}}
```

- `messageId`：设备维护单调自增序号，回包据此对上 in-flight 请求，过期回包丢弃。
- error 用对象 `{code,detail}`，沿用 cloud↔device 既有约定（ADR-019 跨层一致性记录）。
- **firmware 侧注意**：现有 driver picker 走 HTTP 同步，本菜单走 WS 异步——必须改成「发请求 → Loading → 回包到达后重绘」，不能照搬同步 fetch。

**为什么 WS 而非 HTTP**：firmware 在 cloud_saas 下的 HTTP agent API（`/v1/agent/*`）默认经 cloud relay 转发到 home adapter；本菜单恰恰不能 relay。WS 是 cloud 已终结、已认得设备身份的通道，复用它无需另开"不 relay 的 device HTTP 路径 + 设备态鉴权"，最省。

### 3. 鉴权与安全边界

- 设备身份由 WS 握手确定（`device_id`；已绑定即免 token，沿用现状 `handleWS`）。
- cloud 反查 `deviceId → ownerUserID → 该 owner 拥有且已与本设备建立 binding 的 adapter 集合`。
- 切换 = `ActivateBinding`（在已有 binding 间切 active），**绝不调** `CreateBinding`。设备无法绑定新机器。
- 错误码：`NOT_BOUND`（目标未绑定本设备）、`OWNER_MISMATCH`、`ADAPTER_OFFLINE`（语义见 §4）。

### 4. 切换即时生效（路由动态查），离线可切

cloud 路由对每条消息 `resolveDeviceHomeSite` 动态查 active binding，所以切换后**下一条上行消息自动 relay 到新机器，设备无需重连**。

- 允许切到当前离线的 adapter：切换本身成功（active 已变），但 `sites.list` 标 `online:false`，UI 给出离线提示；真正说话时若仍离线，沿用现有 `HOME_ADAPTER_OFFLINE` 错误。
- 切换后设备需**刷新 agent 上下文**：换了机器后 active driver / 当前 session 来自新 adapter，设备应重新拉一次 agent 状态（driver label、session）。

### 5. firmware UI：v1 走本地 picker（ADR-016 模式），标注未来迁 ADR-019

ADR-019 的 firmware `bb_menu_view` 渲染器尚未实现（checklist 末两项未打勾）。要求本功能先做完整套 ADR-019 firmware 迁移，耦合过重、阻塞过久。故：

- **v1**：在 Settings 新增一行 `Adapter`（**仅 cloud_saas 模式显示**），照搬现有 driver picker 的本地渲染形态（`bb_ui_settings.c`）。列表来自 WS `sites.list`，选中发 `sites.activate`。
- **技术债（显式记录）**：待 ADR-019 的 `bb_menu_view` 落地，本 picker 应迁为 cloud 组装的 server-driven 菜单 id（`sites`），与 sessions/drivers 统一渲染。届时这是 ADR-019 的**首个 cloud-assembled 菜单**，反过来验证该协议对 cloud-only 数据的适配。

## 三端改动

### cloud（bbclaw-reference，权威）

- `router/hub.go`：新增设备态 `sites.list` / `sites.activate` 的 kind 处理（终结，不 relay）。
- `account/store.go`：复用 `ListUserHomeAdapters` + bindings；新增/复用 `deviceId → owner + 该设备 bindings` 反查 helper；`ActivateBinding` 复用。
- 组装 site 列表：`label`=`HomeAdapter.DisplayName`，`online` 来自 `hub.Snapshot()` / `IsHomeAdapterOnline`，`active` 来自当前 active binding。
- 单测：列表组装、切换成功/各错误码、离线 adapter 可切。

### firmware（bbclaw）

- `bb_adapter_client.c`：发 `sites.list` / `sites.activate` 控制帧（复用 `bb_adapter_client_send_text`）；`ws_handle_text_message` 加回包解析 case。
- `bb_ui_settings.c`：加 `MAIN_ROW_ADAPTER`（仅 cloud_saas 可见）、adapter picker 渲染（参考 `render_driver_picker`）、`COMMIT_KIND_ADAPTER`。
- 切换成功后触发 agent 状态刷新（driver label / session 重新拉）。
- （可选）NVS cache 当前 `home_site_id` 用于离线展示。

### design

- 本 ADR；在 ADR-019 补一句"新增 cloud-assembled 菜单类别（sites）"的关联；ADR-016 交叉引用。

## 影响

- **正面**：per-device 现场切机器，不用开 Web；完全复用 cloud 既有绑定模型与 WS 通道；授权/使用分层清晰。
- **负面**：再加一个 firmware 本地 picker（逆 ADR-019 方向，已记为债）；cloud 设备态 WS 从"音频 + relay"扩出"控制语义"，需注意鉴权面；跨机器切换后 session/driver 上下文跳变，刷新体验需打磨。
- **中性**：Web portal 切换入口保留，两入口并存——同写 `ActivateBinding`，状态一致。

## 备选方案

1. **只在 Web portal 切（现状）** — 否决：用户明确要设备本地入口。
2. **直接实现 ADR-019 `bb_menu_view`，把 `sites` 作首个 cloud-assembled server-driven 菜单** — 架构最干净、顺带推进 ADR-019，但前置工作量大（整套渲染器 + 菜单栈），与本需求耦合过重。列为 §5 的后续合并路径。
3. **设备态走 cloud-terminated HTTP（`/v1/device/sites`）而非 WS** — 可行，但 firmware cloud_saas 的 HTTP 默认经 relay，要另开不 relay 的 device HTTP 路径 + 设备态鉴权，比复用 WS 控制通道更绕。
4. **设备可直接 claim/绑定新 adapter** — 否决：绑定是管理员动作（ADR-010），设备只在已授权集合内切。

## 实现 checklist

- [ ] cloud: hub 设备态 `sites.list` / `sites.activate`（终结，不 relay）
- [ ] cloud: `deviceId → owner → bindings` 反查 + 列表组装（label/online/active）
- [ ] cloud: `ActivateBinding` 设备态调用 + 错误码（NOT_BOUND/OWNER_MISMATCH/ADAPTER_OFFLINE）+ 单测
- [ ] firmware: WS `sites.list`/`sites.activate` 调用 + 回包解析
- [ ] firmware: Settings `Adapter` 行（仅 cloud_saas）+ picker + commit
- [ ] firmware: 切换后 agent 状态刷新
- [ ] design: ADR-019 / ADR-016 交叉引用更新
- [ ] testing.md: 补 cloud_saas 多 adapter 切换验证步骤
