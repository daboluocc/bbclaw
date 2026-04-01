# BBClaw Beta 路线（官方源码 / adapter / 上游 PR）

## Beta 目标

Beta 阶段目标：

- 稳定跑通 `firmware -> adapter -> OpenClaw` 的闭环
- 把流式音频问题留在 adapter 内解决
- 把与 OpenClaw 的边界稳定在官方 `nodes` 文本/控制面
- 为未来通用上游 PR 保留最小、可解释的切入点

## Beta 成功标准

- 方向正确
  - BBClaw 不再依赖旧插件路线
  - 当前语音链路以 adapter 为音频入口
  - OpenClaw 保持节点文本/控制面角色
- 功能可验证
  - 固件到 adapter 的音频流可验证
  - adapter 到 OpenClaw 的 transcript 注入可验证
  - 展示桥回路可验证
- 工程可延展
  - 文档与实现一致
  - adapter 与 OpenClaw 的边界清晰
  - 未来 PR 候选点具备通用解释

## 路线图

### 阶段 1：当前运行链路稳定

- 稳定 `stream/start/chunk/finish`
- 稳定 ASR provider 接入
- 稳定 `voice.transcript` 注入 OpenClaw
- 稳定 adapter 展示桥

### 阶段 2：控制面边界收敛

- 明确 transcript、reply、display 的边界
- 判断哪些能力仍应留在 adapter
- 判断哪些文本/控制面能力值得抽成未来上游 PR

### 阶段 3：上游候选整理

建议只考虑：

1. 节点 transcript ingress 文档/测试完善
2. 通用回复订阅或控制面事件能力
3. 与当前 adapter 边界一致的最小补丁

## 当前移除项

以下内容不再作为 Beta 主线：

- 直接把 BBClaw 流式音频做进官方 Gateway
- 先推动官方二进制媒体通道再做当前联调
- 双端口插件路线
- `server-plugin/` 继续演进

## 当前优先级

1. P0：adapter 音频流与 ASR 稳定性
2. P0：`voice.transcript` 到 OpenClaw 的稳定性
3. P0：展示桥与设备 UI 联调
4. P1：官方主线可复用能力的识别与整理
5. P2：上游 PR 候选的文档与测试准备
