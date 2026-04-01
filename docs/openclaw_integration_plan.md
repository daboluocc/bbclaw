# BBClaw 与 OpenClaw 集成路线（官方源码 / `nodes` / 上游 PR）

## 目标

- 当前实现先稳定跑通 BBClaw 语音与展示链路
- 与 OpenClaw 的边界尽量保持在官方 `nodes` 模型内
- 未来上游 PR 只推动通用、可解释的文本/控制面能力

## 已确认结论

- BBClaw 的设备语义仍然是硬件节点，不是聊天渠道插件
- 当前可运行实现里，流式音频不再直接进入 OpenClaw Gateway
- `bbclaw-adapter` 负责：
  - 音频流接收
  - ASR/TTS 对接
  - 与固件的展示桥接
- OpenClaw Gateway 负责：
  - 官方 node 握手
  - device pairing
  - node 会话与路由
  - transcript 驱动的 agent 入口

## 当前实施策略

### 运行面区分

- 正式版本：已安装的 `openclaw` CLI / 发布版环境，用于日常使用
- 开发版本：本地 OpenClaw 源码 checkout，用于集成验证与可能的上游 PR

要求：

- 未合并的 BBClaw 相关实验只在开发版本验证
- 不再把正式安装版当成 BBClaw 未发布协议的验证环境

### 当前实际链路

1. 固件通过 LAN HTTP 向 `bbclaw-adapter` 上传音频流
2. adapter 负责缓冲、转码、ASR 调用
3. adapter 以 `role: "node"` 连接 OpenClaw Gateway
4. adapter 向 Gateway 发送 `node.event` / `voice.transcript`
5. Gateway 基于 transcript 驱动 agent 运行
6. adapter / home-adapter 通过 `chat.subscribe` 订阅 `chat(state=delta|final)`
7. BBClaw 展示桥把 assistant 可见文本 delta 流式回送固件；TTS 仍在 final 后触发

### 当前为什么不把流式音频塞进官方源码

- OpenClaw Gateway 目前没有一条足够稳定、足够通用的流式音频节点主线可直接承载 BBClaw
- 如果把 BBClaw 的 streaming/media 细节推进官方源码，改动会很大，PR 解释也会变弱
- 当前 adapter 方案把高变化、高耦合的音频部分留在本仓库，把 OpenClaw 边界收敛到文本与控制面

## 上游 PR 路线

### 可以继续考虑的上游方向

1. 节点文本 ingress 的文档/测试完善
2. `voice.transcript` 这类 node event 入口的通用稳定性
3. 通用的节点回复订阅/回传链路（包括 `chat delta/final`）
4. 确有通用价值时，再讨论 `node.event.send` 这类下行展示/控制能力

### 当前不作为上游主线的方向

- 将 BBClaw 流式音频处理直接并入 Gateway
- 在官方源码内定义 BBClaw 私有二进制音频协议
- 为 BBClaw 再造一套平行于 `nodes` 的设备接入模型

## 当前不再采用的路线

以下路线不再作为主线：

- 本仓库旧的 `server-plugin/` 继续演进
- 私有插件式独立网关路线
- 以“Gateway 直接承载 BBClaw 原始流媒体”为当前第一目标

## 决策标准

如果一个改动同时满足：

- 能让当前 adapter + Gateway 链路更稳
- 与官方 `nodes` 边界不冲突
- 能作为通用文本/控制面能力解释给上游

则优先保留为未来 PR 候选。

如果一个改动必须把 BBClaw 专属 streaming/media 细节压进官方 Gateway，
则默认留在本仓库 adapter 内部解决。
