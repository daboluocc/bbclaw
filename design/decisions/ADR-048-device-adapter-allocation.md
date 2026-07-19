# ADR-048: 设备级 Adapter 分配 — 后台管控设备端可见的 adapter 集合

- **日期**: 2026-07-19
- **状态**: 已实现（cloud+web 已部署生产，bbclaw-reference `6fce78a`，2026-07-19；待真机多设备验证）
- **关联**: ADR-027（设备端切换 Home Adapter）、ADR-010（授权层/使用层分层）、ADR-025（web 优先配置）、ADR-011（cloud 闭源，绑定模型在 cloud）

## 背景

ADR-027 落地后，设备端 Settings 里已有 Adapter picker（WS `sites.list` / `sites.activate`）。
但实现时（bbclaw-reference issue #33）把可切换集合从原设计的"已绑定到本设备的 adapter"
**放宽为 owner-level 全可见**：

- `Hub.ListDeviceSites`（`cloud/internal/router/sites.go:70-116`）直接返回
  `ListUserHomeAdapters(ownerID)` —— **账号下全部 adapter**；
- `Hub.ActivateDeviceSite`（`sites.go:126-156`）调 `CreateBinding` upsert —— 设备可
  即时"绑定 + 激活"账号下任意 adapter，无需预授权；
- `Binding` 实际语义已退化为**每设备一条的 active 指针**（`CreateBinding` 先清掉该
  设备全部旧 binding 再插入一条，`account/store.go:1154-1176`），不再是授权集合。

当时放宽是对的：账号下只有一两台 adapter，预绑定纯属摩擦。

**现在的问题**：设备用途开始分化 —— 同一账号下会同时挂**写码的 Mac、门禁/对讲的
K230 网关、跑本地 Ollama 的 NAS、公司机器**等多台 adapter。全可见模型下：

1. 每台设备的 picker 都列出全部 adapter，与该设备用途无关的占多数，列表污染；
2. 设备可以误切到完全不相干的 adapter（门禁对讲机切到写码 Mac），没有任何后台管控；
3. 固件 picker 行池上限 16 行（`bb_ui_settings.c` `s_st.rows[16]`），adapter 多了放不下。

**需求**：后台（web portal）能**按设备分配 adapter**——管理员决定每台设备"看得见、
切得动"哪些 adapter；设备端 picker 只展示分配给它的子集。

## 决策

### 1. 定位：把 ADR-027 丢掉的"授权层"找回来，但做成可选白名单

ADR-010/027 的分层是：**授权（管理员，Web）/ 使用（用户，设备）**。issue #33 实际上
删掉了授权层。本 ADR 恢复它，但吸取教训——不回到"必须预绑定"的强制模式：

> **每台设备有一个"分配集合"（allocation）。集合为空 = 未配置 = 全部可见（现状，
> 零配置开箱即用）；集合非空 = 白名单，设备只能看见/切换集合内的 adapter。**

新账号、单 adapter 用户体验不变；多用途账号按需收紧。

### 2. 数据模型（cloud `account.Store`，闭源仓）

新增与 `Bindings` 平行的持久化列表：

```go
// Allocation 表示"某设备被允许使用某 adapter"（后台分配的可见性白名单条目）。
// 某设备一条 Allocation 都没有 = 未配置 = 该设备可见账号下全部 adapter。
type Allocation struct {
    DeviceID   string `json:"deviceId"`
    HomeSiteID string `json:"homeSiteId"`
    CreatedAt  string `json:"createdAt"`
}
```

- 落盘沿用 `pairings.json`（空列表对旧数据零迁移，无需 schema bump）。
- `Binding` 保持现语义不动（per-device active 指针）。**分配（可见集合）与激活
  （当前指向）是两层**，不复用同一张表——复用会重蹈 issue #33 的语义混叠。
- Store 方法：
  - `ListDeviceAllocations(deviceID) []Allocation`
  - `SetDeviceAllocations(userID, deviceID, homeSiteIDs []string) error`
    ——整集合替换、幂等；逐个过 `validateOwnershipLocked`（device 与 adapter 必须
    同属该 user）。
  - `DeleteHomeAdapter` / `ReleaseDevice` 级联清理相关 Allocation（与 Binding 同待遇）。

### 3. 不变式：active adapter 永远在可见集合内（写时维护，读时不查）

路由热路径 `resolveDeviceHomeSite()` **不改**——仍只读 active binding。管控在两个
写入口维护不变式：

| 写入口 | 规则 |
|---|---|
| `SetDeviceAllocations`（后台保存分配） | 若新集合非空且当前 active 不在集合内：**自动把 active 重指到集合第一个 adapter**（`CreateBinding`）。设备无感存活，不会陷入 `BINDING_REQUIRED`。portal 保存前弹提示"将把设备切到 X"。 |
| `CreateBinding`（portal 手动绑定） | 若该设备分配集合非空且目标不在集合内：**自动把目标追加进集合**。管理员显式绑定即隐含分配意图，且维持"active ∈ 可见集合"。 |

设备端 `ActivateDeviceSite` 则是**校验方**：目标不在非空分配集合内 → 拒绝，新错误码
`NOT_ALLOCATED`（加入 ADR-027 错误码全集；固件对未知 code 走通用错误展示路径，无需
改固件）。

### 4. `sites.list` 过滤（cloud `router/sites.go`）

`ListDeviceSites` 组装前先查分配：

```go
allocs := h.accounts.ListDeviceAllocations(deviceID)
// allocs 为空 → homes 不过滤（现状）；非空 → homes 只保留集合内的
```

**回包契约（`{sites:[{homeSiteId,label,online,active}]}`）逐字不变**，只是元素变少。
这是本设计最重要的收益：**固件零改动、零 OTA、无 tag**——老固件自动只看到分配子集。

### 5. Web portal：AdapterTab 从"绑/解绑"升级为"分配面板"

`web/src/pages/device-tabs/AdapterTab.jsx` 重做为每设备的分配管理页：

- **adapter 复选列表**：账号下全部 adapter（名称 + 在线 pill + homeSiteId），勾选 =
  分配给本设备；全不勾 = 未配置（全部可见），页面明示这一语义。
- **active 单选**：勾选行内的 radio 标"当前使用"，即现有 `activateBinding` 能力并入
  同一面板。
- 保存 = `PUT /v1/allocations`（整集合替换）+（active 变更时）`POST /v1/bindings/activate`。
- 保存时若 active 被移出集合，前端提示将自动切换（§3 规则）。

HTTP API（`portal_api.go` + `server.go` 注册，风格沿用现有 query-param 式）：

| 方法 | 端点 | 语义 |
|---|---|---|
| `GET /v1/allocations?deviceId=` | 查询 | 返回 `{homeSiteIds:[...]}`；空数组 = 未配置 |
| `PUT /v1/allocations` | 保存 | body `{deviceId, homeSiteIds:[...]}`，整集合替换；`[]` = 清除配置回到全可见 |

### 6. 非目标（显式排除）

- **adapter 的"用途"元数据**（purpose/标签/分组）：DisplayName 已足够表达
  （"门禁-东门"、"写码-家里 Mac"）。若将来 adapter 数量大到需要按用途分组筛选，
  再增量加 `HomeAdapter.Purpose`，与本 ADR 正交。
- **设备端管理分配**：分配是管理员动作，只在 Web（ADR-010 分层）。设备端仍只有
  "在分配集合内切 active"。
- **local_home 模式**：直连固定 IP，无此概念（同 ADR-027）。

## 三端改动

### cloud（bbclaw-reference，闭源）

- `account/store.go`：`Allocation` 结构 + `ListDeviceAllocations` / `SetDeviceAllocations`
  + 级联清理 + §3 两条写时规则。
- `router/sites.go`：`ListDeviceSites` 过滤；`ActivateDeviceSite` 加 `NOT_ALLOCATED` 校验。
- `httpapi/portal_api.go` + `server.go`：`GET/PUT /v1/allocations`。
- 单测：空集合=全可见、白名单过滤、`NOT_ALLOCATED`、active 被移出自动重指、
  portal 绑定自动追加分配、级联清理。

### web（bbclaw-reference，闭源）

- `AdapterTab.jsx` 重做为分配面板；`api.js` 加 `allocations()` / `setAllocations()`。

### firmware（bbclaw，本仓）

- **无改动**。`sites.list` / `sites.activate` 契约不变；`NOT_ALLOCATED` 走现有未知
  错误码通用路径。

### design

- 本 ADR；ADR-027 补交叉引用（"可切换集合的管控见 ADR-048"）。

## 影响

- **正面**：多用途设备各看各的 adapter；后台获得管控面；固件零改动零发版；
  空集合默认全可见，存量用户无感。
- **负面**：cloud 状态多一张表、两条写时不变式规则（复杂度集中在写路径，读路径零开销）；
  分配与绑定两个概念需要在 portal 文案里讲清楚（"可见" vs "当前使用"）。
- **中性**：ADR-027 文中"设备只能在已绑定集合内切"的原始表述，经 issue #33 + 本 ADR
  后最终落为"设备只能在**分配集合**（未配置则为全账号）内切"。

## 备选方案

1. **恢复多条 Binding 作授权集合（ADR-027 原设计）** — 否决：`CreateBinding` 的
   upsert 语义已在生产（含设备端 `ActivateDeviceSite` 调用路径），改回 append 语义
   牵动路由回退逻辑（`resolveBoundHomeSiteLocked` 的 fallback），风险大于新增一张
   语义单一的表。
2. **Device 上加 `AdapterScope: all|allocated` 模式字段 + 集合** — 与"空集合=全可见"
   表达力等价但多一个字段、多一种非法状态（scope=allocated 且集合空）。否决。
3. **在固件端过滤（云端下发全量 + 可见性标记）** — 需要改固件、发 OTA，且设备端
   过滤不构成管控（协议上仍可激活任意 adapter）。否决。
4. **按"用途"给 adapter 打标签，设备声明用途自动匹配** — 间接层过重；分配关系
   显式列出比标签匹配可预测。列为将来分组筛选的候选，非本次。

## 实现 checklist

- [x] cloud: `Allocation` 模型 + store 方法 + 级联清理 + 单测（`account/store.go`）
- [x] cloud: `ListDeviceSites` 过滤 + `ActivateDeviceSite` `NOT_ALLOCATED` + 单测（`router/sites.go`、`sites_test.go` +9 例全绿）
- [x] cloud: 写时不变式（分配保存重指 active / portal 绑定追加分配）+ 单测
- [x] cloud: `GET/PUT /v1/allocations` + 路由注册（`portal_api.go` + `server.go`）
- [x] web: AdapterTab 分配面板 + api.js（复选可见集合 + 设为当前 + 移出 active 确认提示）
- [x] design: ADR-027 交叉引用更新
- [x] 部署生产（bbclaw-reference `6fce78a`，`make deploy-all`，healthz ok）
- [ ] 部署后真机验证：两台设备各配不同分配集合，picker 各自只见子集，跨集合激活被拒
