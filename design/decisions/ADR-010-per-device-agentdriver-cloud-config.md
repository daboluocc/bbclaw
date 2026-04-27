# ADR-010: Per-device AgentDriver 作为云配置（v0.4.0 多 driver）

- **日期**: 2026-04-27
- **状态**: 已接受
- **关联**: ADR-001（adapter 作为 Agent Bus）、ADR-003（Router + 多
  driver 路由）、ADR-004（cloud_saas 模式 Agent Bus 代理）、ADR-005
  （openclaw 接入 AgentDriver）

## 背景

v0.4.0 把 adapter 端 Agent Bus 扩到 4 个 driver：`claude-code` /
`openclaw` / `ollama` / `opencode`。Router（ADR-003）的输入需要一个
"本次请求该用哪个 driver" 的字段，否则只能走默认。

local_home 模式里这个字段简单：firmware 直接发到本机 adapter，adapter
自己有配置文件能决定 default driver；用户改 driver 改的是本地配置。

cloud_saas 模式（ADR-004）变复杂：

- 一个 cloud 服务挂多个设备
- 每个设备的所有者可能想用不同 driver（A 用户买了 Claude API 走
  claude-code；B 用户本地跑 ollama；C 用户用免费 openclaw）
- 设备本身不应该背 "driver 是谁" 的负担——固件 OTA 升级换 driver
  名单的话太重
- 设备 UI 上没有合适位置让用户随时切（Settings overlay 有了，但
  cloud_saas 的 driver 列表是云端动态的，固件不知全集）

需要一个机制：**设备只发"想说话"，由云查它的配置决定路给谁**。

## 决策

### 1. Device.Config 加 `agentDriver` 字段

```go
// bbclaw-reference/cloud/internal/device/config.go
type Config struct {
    // ... 既有字段（wifi/ssid 等）
    AgentDriver string `json:"agentDriver,omitempty"`
}
```

`omitempty`：未设置时序列化为空，云端逻辑视空为"用 adapter 默认"，
保持向后兼容老设备。

存储与既有 device config 同表 / 同文档（device id → config），不
新建结构。

### 2. Web portal 暴露 driver 下拉

Web portal（`bbclaw-reference/cloud/internal/...` 的 React 部分）
在设备详情页加 driver 下拉。**列表来源**：

- 不写死前端
- 调 adapter 的 driver list 接口（commit `0bd2dd4` 引入），拿当前
  adapter 实际 register 了哪些 driver
- 渲染下拉 + "currently active: X"

为什么从 adapter 取而不是 cloud 自己存：driver 的 source-of-truth 是
adapter（哪个二进制版本支持哪些 driver），cloud 仅做"per-device 选了
哪个"的存储层。版本升级 adapter 加新 driver，web portal 自动跟上。

### 3. 上行链路注入 driver

cloud_saas 上行有两条主路径：

| 路径 | 入口 | 注入位置 |
|---|---|---|
| 语音 | `voice.transcript` 事件（设备发音频→cloud ASR→cloud forward） | cloud 在 forward 给 adapter 时，`Payload["driver"] = device.Config.AgentDriver` |
| 文本 | `chat.text` 事件（未来；同样从 device 上行） | 同上，cloud 注入 |

adapter 端的 `handleTranscriptRequest`（`adapter/internal/httpapi/server.go`）
读 `Payload["driver"]`，传给 router；router 按 ADR-003 规则选择
具体 driver 实例。

**关键**：固件完全不发 driver 名。固件发的事件载荷里没有 driver 字
段，也不读云端配置回传 driver。固件对 driver 的认知仅限于"当前显示
什么名字"——这个名字来自 adapter session 建立时回传的 driver name
（用于 topbar 展示），不是固件主动选的。

### 4. 配置生效路径

```
用户在 web portal 选 driver
    ↓
cloud 写 Device.Config.AgentDriver
    ↓
下次 voice/chat 事件经过 cloud 时，cloud 读取并注入 Payload
    ↓
adapter 收到带 driver 的请求 → router 路由
```

不做"立即推送给设备"。原因：

- 设备不需要知道 driver 是谁（topbar 展示由 session 回包驱动）
- 推送增加 cloud → 设备的反向通道复杂度
- 用户改完下次说话即生效，体感差异 < 1 个对话轮次

### 5. 配置 vs 消息：为什么是 config-driven

考虑过让消息携带 driver（设备 UI 选完直接塞进 PTT 上行）。否决理由：

- 设备 UI 不知道 cloud 当前可用 driver 全集（cloud 可能配了某些
  driver、adapter 可能没编译进去）
- 切 driver 是 "管理员" 动作（谁付钱用谁的 API），不是 "用户" 动作；
  应在管理面（web portal）做，不在设备做
- Config-driven 跨重启持久（NVS / 设备本地都不需要存）
- 多设备共享一套云端配置时（家庭/团队），管理员一处改所有设备同步

## 后果

### 正面

- 设备固件简单：完全不管 driver
- 管理员体验清晰：web portal 是单一管理面
- 跨重启 / 跨设备 / 跨固件版本一致
- adapter 加新 driver 时，web portal 自动暴露
- 与 ADR-003 的 router 解耦良好：router 只看 Payload，不关心 Payload
  来源

### 负面 / 风险

- 设备和云端的 driver 状态可能不一致（云改了，设备 topbar 还显示
  老 driver）。当前仅在下次 session 建立时同步——用户感知较弱，
  接受
- Web portal 必须能访问 adapter 的 driver list 接口；adapter 离线时
  下拉为空。需做"上次成功获取的列表" 缓存，未做
- 测试覆盖弱：cloud 注入 Payload 这步没有 e2e 测试，只有人工 web
  改 + 真机说一句验证
- 未定义：若 adapter 配置里没注册云端选的 driver 名（典型：adapter
  降级到老版本）会怎样？目前 router fallback 到默认 driver，不报
  错；用户感觉"切了没生效"。需加日志 + 后续告警

### 未来工作

- 定义 firmware NVS-stored `selected_driver` 与 cloud-config 的冲突
  规则：当前 cloud_saas 模式下 cloud 完胜（firmware 不发也不存）；
  local_home 模式下 firmware 自己存，无冲突
- Web portal 加"当前生效 driver" 实时反馈（当前只显示设置值，不显
  示 adapter 实际是否接受）
- adapter driver-list 接口加 health 字段（这个 driver 当前可用吗？
  Claude API 余额是否够？）让 web portal 能 disable 不可用项
- 多租户：driver 配置可能要从 device 维度上升到 user 维度，多个设
  备共享同一个 user 的 driver 偏好

## 触发何时撤销

- 如果出现"管理员只想给所有设备统一改 driver"的强需求，加一层
  "tenant default driver"，device 字段降为可选 override，不撤销本
  ADR
- 如果 cloud → device 反向通道（OTA / 配置推送）建好了，可以考虑
  把 driver 配置变更推到设备 topbar 实时刷新；但路由层仍由 cloud
  注入，不改本 ADR 的核心
