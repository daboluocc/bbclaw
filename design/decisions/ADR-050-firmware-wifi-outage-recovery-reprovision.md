# ADR-050: WiFi 断网恢复与重新配网流程——运行期阶梯降级 + 免重启热切网

- **日期**: 2026-08-01
- **状态**: 提议（待评审）。docs-only，尚未写码。
- **组件**: firmware（纯固件；cloud / adapter **零改动**，无跨组件契约变更）
- **关联**:
  - [[ADR-039]] 密语离线死锁——同一「断网鲁棒性」家族，本 ADR 把断网语义从密语一处扩到全局
  - `design/STATE_MACHINE.md` §3.5.1 NETCONN 页 / §3.5.2 配网页——本 ADR **修正**其中一处与代码不符的描述（见 §7）
  - 现有实现：`firmware/src/bb_wifi.c`（1417 行）、`bb_page_netconn.c`、`bb_page_apconfig.c`
  - 历史 issue：#14（配网态后台重连免重启）、#163（首连 SSID 自动入库）、#170（运行期 backoff 重连）、#182（scan 预筛）、#252（softAP 内部 DMA boot-loop 守卫）

## 1. 背景

### 1.1 已经有的（不要重复造）

`bb_wifi.c` 已经不是空白，盘点清楚才能只补缺口：

| 能力 | 位置 | 说明 |
|---|---|---|
| 多网络凭据槽 | `load/save/delete_wifi_slot` | NVS 存 `BBCLAW_WIFI_MAX_SAVED=8` 组，带全局递增 `conn_seq` 当「最近成功」时间戳 |
| 开机按最近成功排序尝试 | `find_reachable_saved_network` + boot 流程 | 先 scan 预筛（#182）再按 rank 逐个试，全失败才进门户 |
| 运行期快速重试 | `on_wifi_event` STA_DISCONNECTED | `BBCLAW_WIFI_STA_MAX_RETRY=2` 次立即 `esp_wifi_connect()` |
| 运行期指数退避 | `reconnect_timer_*` | 5s 起翻倍，**封顶 300s**，`BB_WIFI_MODE_STA_RECONNECTING` |
| SoftAP 配网门户 | `start_ap_provisioning_mode` + httpd | captive DNS、扫描列表、保存/删除、`BBClaw-Setup-<MAC>` / `bbclaw1234` |
| 配网态后台自动重连 | `provision_retry_task`（#14） | 门户挂着的同时每 30s 重扫，保存过的网一回来自动连上、免重启 |
| softAP 内存守卫 | `BBCLAW_WIFI_AP_MIN_DMA_*`（#252） | 内部 DMA 堆不足时优雅返 `NO_MEM`，不让闭源 wifi 库崩成 boot loop |
| 调试注入 | `bb_wifi_debug_enter_provisioning`（devmon 202） | 不拔路由器也能复现配网链路 |

### 1.2 真正的缺口（本 ADR 要解决的）

**G1 — 运行期掉线永远回不到配网门户。**
`start_ap_provisioning_mode()` 全仓只有三个调用点：boot 流程全部尝试失败（`bb_wifi.c:1372`）、
配网态内部重入（`:1027`）、devmon 调试注入（`:1385`）。**运行期掉线没有任何一条通往门户的路**。
换路由器、改密码、搬家之后，设备只会以最长 5 分钟一次的节奏、**对着那个已经不存在的 SSID 重连到天荒地老**。
用户在设备上无法自助，只能拔电重启走 boot 流程。

**G2 — 退避重连不轮换其它已保存网络。**
`reconnect_timer_cb` 只调 `esp_wifi_connect()`，用的是当前 `wifi_config` 里那一个 SSID。
开机路径辛苦维护的 8 槽 + 最近成功排序，**在运行期完全用不上**。家里双频段切换、
主路由重启后设备落到备用网、AP 漫游——这些最常见的场景都恢复不了。

**G3 — 设备上没有「重新配网」入口。**
`bb_ui_settings.c` 的 `MAIN_ROW_*` 里没有任何 WiFi/网络行。运行期进配网的唯一入口是
devmon 调试注入——那是给 AI/开发者的串口通道，不是给用户的。

**G4 — 落地配网必然整机重启。**
`STATE_MACHINE.md` §3.5.2 明写配网页「只在用户提交凭据后由 `bb_wifi` 的 `esp_restart()` 收场，
**无自销毁路径**」。但 #14 已经做出了「后台重连成功 → 免重启 → dismiss 配网页」的路径
（`bb_radio_app.c:3936` 那段收尾）。于是同一个「网好了」事件有两套语义：自动恢复免重启、
手动配网必重启。重启会丢掉会话上下文与未播完的回复，对「换个网继续聊」是不必要的代价。

**G5 — 断网期间的应用层语义没有成文。**
运行期掉线时 `bb_radio_app.c:3944` 只做了三件事：`show_status_error("RECONNECTING")`、
`s_transport_health_ok = 0`、暂停心跳。至于**断网时按 PTT 会怎样、正在播的 TTS 怎么收场、
正在跑的 agent turn 算不算被打断、OTA/ambient 上传怎么处理、恢复后要不要续上**——
没有任何设计约束，各处各写各的。ADR-039 只覆盖了密语锁屏这一个切面。

## 2. 目标 / 非目标

**目标**
1. 运行期任何网络故障，设备都能**自己走到一个用户可介入的状态**，不需要拔电。
2. 换网/改密码后，用户能在**设备上**发起重新配网，并且**不重启**就切过去。
3. 断网期间设备行为可预期、成文，不假装在线。
4. 全部在现有内存预算内，尤其不触碰 #252 那颗 softAP DMA 地雷。

**非目标**
- 不做 BLE / SmartConfig / 一键配网（SoftAP 门户已够用，且零新增依赖）。
- 不做网络诊断上报云端（本 ADR 只管设备自愈）。
- 不改 cloud / adapter 任何契约。
- 不处理「WiFi 连着但云端不可达」——那是 `s_transport_health_ok` 的领域，与本 ADR 正交。

## 3. 决策：把运行期掉线做成**四级阶梯**，而不是一个死循环

核心决策一句话：**运行期掉线不再是「退避重连当前 SSID」一种状态，而是一条会
自己往下走、每一级都有明确超时和出口的阶梯，最后一级是用户可介入的门户。**

```
        STA_CONNECTED
             │ STA_DISCONNECTED
             ▼
  ┌─ L1 快速重试 ×2 ────────────────┐  当前 SSID，立即
  │        ↓ 耗尽                    │
  ├─ L2 退避重连 5s→10s→…→60s ──────┤  当前 SSID，累计 T1=90s
  │        ↓ 超 T1                   │  ← 现状停在这里，且封顶 300s
  ├─ L3 全网重扫轮换 ───────────────┤  scan + 按最近成功排序试 8 槽
  │        ↓ 超 T2=5min 或轮空 2 轮  │
  └─ L4 门户回落(APSTA) ────────────┘  开 AP 门户 + 后台每 30s 继续重扫
             │ 任一级 GOT_IP
             ▼
        STA_CONNECTED（免重启，发 BB_EVT_NET_UP）
```

四个关键决策：

- **D1｜退避上限从 300s 降到 60s。** 上限存在的意义是省电，但在阶梯里它只需要撑到 T1；
  300s 会让 L2 单级就吃掉 5 分钟，阶梯失去意义。
- **D2｜L3 复用开机那套 `find_reachable_saved_network`**，不新写选网逻辑——直接消掉 G2，
  且保证「开机怎么选网、运行期就怎么选网」语义统一。
- **D3｜L4 用 `WIFI_MODE_APSTA` 而不是纯 AP。** 现状 `start_ap_provisioning_mode()` 先
  `esp_wifi_stop()` 再起纯 AP，代价是**关掉 STA 就没法后台重连**，#14 才不得不额外起一个
  `provision_retry_task` 反复 start/stop STA。APSTA 下门户和后台重扫天然共存。
  **但这一条受内存门槛制约**——见 §6 的降级约定。
- **D4｜运行期配网提交后不重启**（G4）。停 AP → 用新凭据连 STA → 成功即 dismiss 配网页 +
  `BB_EVT_NET_UP`，复用 #14 已经跑通的收尾路径。**boot 期配网仍保留 `esp_restart()`**：
  开机态没有值得保住的上下文，重启换来干净的初始化，不值得为它多一条路径。

## 4. 详细设计

### 4.1 状态与阈值

`bb_wifi_mode_t` 扩两个值（现有三个不动，保证既有调用点语义不变）：

```c
typedef enum {
  BB_WIFI_MODE_NONE = 0,
  BB_WIFI_MODE_STA_CONNECTED = 1,
  BB_WIFI_MODE_AP_PROVISIONING = 2,
  BB_WIFI_MODE_STA_RECONNECTING = 3,  /* L1+L2：当前 SSID 重试中 */
  BB_WIFI_MODE_STA_ROAMING = 4,       /* L3：扫描并轮换已保存网络 */
  BB_WIFI_MODE_AP_FALLBACK = 5,       /* L4：门户 + 后台重扫并存(APSTA) */
} bb_wifi_mode_t;
```

> `bb_wifi_is_provisioning_mode()` 需同时认 `AP_PROVISIONING` 与 `AP_FALLBACK`，
> 否则 `bb_radio_app.c` 里既有的配网态分支会漏掉回落场景。

新增可 Kconfig 覆盖的阈值（放 `bb_config.h`，与现有 WiFi 常量同段）：

| 常量 | 默认 | 含义 |
|---|---|---|
| `BBCLAW_WIFI_RECONNECT_BACKOFF_MAX_MS` | **60000**（现 300000） | L2 退避上限，D1 |
| `BBCLAW_WIFI_L2_TIMEOUT_MS` | 90000 | L2 累计多久后升 L3 |
| `BBCLAW_WIFI_L3_TIMEOUT_MS` | 300000 | L3 累计多久后升 L4 |
| `BBCLAW_WIFI_L3_MAX_SWEEPS` | 2 | L3 连续轮空几轮直接升 L4（比超时先到就先生效） |
| `BBCLAW_WIFI_L4_RESCAN_MS` | 30000 | L4 后台重扫周期（沿用 #14 的 30s） |

计时基准复用已有的 `s_reconnect_start_ms`，不新增时间源。

### 4.2 L3：全网重扫轮换

进入条件：L2 累计 ≥ `L2_TIMEOUT_MS`。行为：

1. `s_suppress_autoconnect = 1`（复用 #182 的预筛开关，避免扫描期事件回调乱插手）
2. `wifi_scan_collect()` 拿到可见 AP 列表
3. 与 8 个 NVS 槽求交集，按 `conn_seq` 倒序（最近成功优先）
4. 逐个 `start_sta_connection()`；成功 → `GOT_IP` 正常收敛回 `STA_CONNECTED`
5. 一轮全败 = 一次 sweep；`L3_MAX_SWEEPS` 轮空或累计超 `L3_TIMEOUT_MS` → 升 L4
6. 每轮之间 sleep `BACKOFF_MAX_MS`，别把扫描做成耗电风扇

**复用而非重写**：4.2 第 2–4 步就是 `find_reachable_saved_network()` 现有逻辑，
只需把它从「boot 一次性调用」抽成可被运行期反复调用（去掉对 `s_mode` 初值的隐含假设）。

### 4.3 L4：门户回落（APSTA 并存）

```
esp_wifi_set_mode(WIFI_MODE_APSTA)
  ├─ AP 侧：SSID/密码/IP 与现有 start_ap_provisioning_mode 完全一致（用户看到的东西不变）
  │         httpd + captive DNS 照旧
  └─ STA 侧：保持一个 L4_RESCAN_MS 周期的重扫任务（等价 #14 的 provision_retry_task，
             但不再需要 start/stop STA，只需 scan + connect）
```

- 进 L4 **前**必须先过 §6 的 DMA 门槛检查（复用现有 `BBCLAW_WIFI_AP_MIN_DMA_*`）。
- 进 L4 时 UI 切到配网页（`bb_page_apconfig_show()`），文案与首启配网**共用一套**，
  只在标题加一行状态：`原网络已断开`。
- **L4 不是终点**：后台重扫成功照样自动回 `STA_CONNECTED`、dismiss 配网页——
  这正是 #14 已经验证过的收尾路径，直接沿用。

### 4.4 运行期配网提交：免重启热切网（D4）

`handle_configure_post` 现在的收尾是保存凭据 → 回 "Device will reboot" → `restart_task`。
改成按来源分叉：

| 来源 | 行为 |
|---|---|
| boot 期配网（`AP_PROVISIONING`） | **保持现状**：保存 → 提示重启 → `esp_restart()` |
| 运行期回落（`AP_FALLBACK`） | 保存 → 回「已保存，正在切换到 <ssid>」→ 停 AP + httpd + DNS → `start_sta_connection(新凭据)` → 成功：dismiss 配网页 + `BB_EVT_NET_UP`；失败：**回到 L4 门户**并在页面上回显失败原因 |

> 热切网失败要能回门户，否则用户输错密码就把自己关在门外了——这是 D4 唯一的硬要求。

### 4.5 设置页「网络」入口（G3）

`MAIN_ROW_*` 新增 `MAIN_ROW_NETWORK`（排在 `MAIN_ROW_SYSINFO` 之前），进二级页：

```
网络
  当前   <ssid>  <rssi>dBm        ← 未连接时显示当前处于 L1/L2/L3/L4 哪一级
  重新配网                        ← 双击确认，主动进 L4 门户（复用同一条路径）
  已保存网络 (n/8)                ← 列表，可逐个「忘记」(delete_wifi_slot)
  返回
```

- 「重新配网」= 用户主动触发 L4，不必等阶梯自己走到底。**双击确认**防误触
  （沿用 `MAIN_ROW_RECORDER` 已有的防误进模式）。
- 「忘记」直接调现有 `delete_wifi_slot`，无新逻辑。
- 该页在所有板子上都要能用**按键**操作（M5StickS3 只有侧键、bbclaw 无触屏），
  不得只做触摸交互。

### 4.6 断网期间的应用层语义（G5）

统一约定，写进 `bb_radio_app.c` 并在 STATE_MACHINE.md 落档：

| 事项 | 断网期（L1–L4 任一级）行为 |
|---|---|
| PTT 按下 | **拒绝并提示**「网络未连接」，不录音、不排队。理由：排队会让用户以为发出去了，恢复后又冒出一段莫名其妙的旧话 |
| 正在跑的 agent turn | 标记 interrupted（沿用 ADR-028 §2.5.1 打断语义），不静默丢弃 |
| 正在播的 TTS | 播完本地已缓冲部分即停，不硬切（听感上「说完这句」比「戛然而止」好） |
| OTA 检查 | 暂停；恢复后由既有心跳节奏自然重新触发 |
| ambient 上传（ADR-044） | 暂停上传、**继续本地落盘**，恢复后补传 |
| 密语锁屏 | 沿用 ADR-039：断网不进锁屏、已锁自动解锁 |
| 顶栏 | WiFi 图标反映真实等级，见 4.7 |

### 4.7 UI 状态

现状运行期掉线只有一句 `show_status_error("RECONNECTING")`，四级看起来一模一样。改为：

| 级 | 顶栏状态文本 | WiFi 图标 |
|---|---|---|
| L1/L2 | `RECONNECTING` | 空心闪烁 |
| L3 | `SEARCHING` | 空心 + 扫描动效 |
| L4 | （切配网页，顶栏让位） | AP 广播弧（复用 apconfig 页现有动画） |

## 5. 内存与风险

- **R1｜softAP DMA 地雷（#252）。** L4 在运行期开 AP，此时内部 DMA 堆比 boot 期更碎
  （LVGL、TLS、音频都已占用），比首启配网更危险。**必须**在进 L4 前跑现有那道
  `BBCLAW_WIFI_AP_MIN_DMA_FREE/LARGEST` 检查。
  **降级约定**：门槛不过 → **不进 L4**，退回 L3 无限轮换，并在顶栏显示 `NET LOW MEM`，
  同时日志打出确切水位。宁可少一个门户，也不能让 wifi 库崩成 boot loop。
- **R2｜APSTA 比纯 AP 更吃内存。** 若真机实测 APSTA 在运行期站不住，退化方案 = L4 仍用
  纯 AP + 保留 #14 的 `provision_retry_task`（功能等价，只是多一个任务、多几次 start/stop）。
  **这是 D3 的兜底，不影响本 ADR 其余部分。**
- **R3｜扫描耗电。** L3/L4 的周期性 scan 与 ADR-047 的 CPU light-sleep 有交互，
  电池供电的板（手表 / M5StickS3）需实测；必要时按电源状态拉长 `L4_RESCAN_MS`。
- **R4｜阈值都可 Kconfig 覆盖**，真机调参不必改码。

## 6. 分期落地

| 阶段 | 内容 | 闸门 |
|---|---|---|
| **P0** | L2 上限降 60s + L3 全网轮换（消 G2）；`STA_ROAMING` 态 + 顶栏 `SEARCHING` | 拔路由器/改密码真机验证：设备能落到备用网 |
| **P1** | L4 门户回落 + DMA 门槛降级（消 G1）；设置页「网络」入口（消 G3） | devmon 注入 + 真机断网 5 分钟，确认自动进门户且能救回 |
| **P2** | 运行期免重启热切网（消 G4）；4.6 应用层语义统一落地（消 G5） | 换网后会话上下文不丢；断网按 PTT 有明确提示 |

**验收 checklist（真机，实战派 + M5StickS3 各跑一遍）**
- [ ] 主路由断电 → 设备 90s 内进 L3，落到备用网
- [ ] 所有已保存网络都不可达 → 5 分钟内进 L4，手机能连上 `BBClaw-Setup-*` 打开门户
- [ ] 门户里填新网 → **不重启**切过去，配网页自动消失，能立刻对话
- [ ] 门户里填错密码 → 回到门户并显示失败，不失联
- [ ] 断网期间按 PTT → 明确提示，不录音、恢复后不冒旧话
- [ ] 设置页「重新配网」双击 → 进门户；「忘记」→ 槽位真的少一个
- [ ] 低内存下进 L4 被拒 → 顶栏 `NET LOW MEM`，**不 boot loop**（#252 回归）

## 7. 对既有文档的修正

`design/STATE_MACHINE.md` §3.5.2 现在写配网页触发条件是
「首启无凭据 / **运行中 WiFi 掉线回落**」——**后半句与代码不符**（G1：运行期根本没有这条路）。
本 ADR 落地 P1 后该描述才成立。在 P1 合入前，应先把该句改成「首启无凭据（运行期回落见 ADR-050，未实现）」，
避免文档继续描述一个不存在的行为。同节「只在提交凭据后由 `esp_restart()` 收场，无自销毁路径」
在 P2 后需按 4.4 更新。

## 8. 备选方案与不采纳理由

| 方案 | 不采纳理由 |
|---|---|
| 掉线直接进门户（跳过 L2/L3） | 路由器重启、AP 漫游这类**几秒到几十秒**的抖动极常见，直接弹门户等于把正常波动当故障，体验倒退 |
| 掉线 N 次后 `esp_restart()` 走 boot 流程 | 能复用现有 boot 选网逻辑、改动最小，但**丢会话上下文**、开机动画重来一遍，且掩盖真实故障（日志断在重启点）。L3 用同一套选网逻辑就能拿到同样效果，没必要重启 |
| BLE / SmartConfig 配网 | 新增依赖与 flash 占用（相机板已在 2.5MB 边缘），且 SoftAP 门户是所有手机都能用的最大公约数 |
| 把重连策略挪到云端下发 | 断网时恰恰拿不到云端配置——自愈逻辑必须完全本地自洽 |
