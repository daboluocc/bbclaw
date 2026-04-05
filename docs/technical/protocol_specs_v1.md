# BBClaw over OpenClaw Nodes Protocol (Historical Draft)

Version: 1.1.0  
Status: Archived draft  

## 说明

这份文档保留为历史背景，用于记录项目曾经尝试过的
“BBClaw 更深地贴近 OpenClaw `nodes` 控制面”的设计方向。

它不再代表当前主实现。

当前主实现请以以下文档为准：

- `docs/architecture.md`
- `docs/protocol_specs.md`
- `src/README.md`

## 历史方向摘要

当时的设计假设是：

- BBClaw 设备更直接地贴近官方 node transport
- 下行能力优先围绕 `node.event.send`
- 音频未来可能继续沿 Gateway 内 node media 能力推进

## 为什么归档

当前实现已经调整为：

- 流式音频由仓库内 `bbclaw-adapter` 负责终止
- adapter 对接 ASR 后，只把 transcript 文本送入 OpenClaw
- OpenClaw 保持在官方 node 文本/控制面边界

因此，这份文档中的以下判断已不再是当前主线：

- `node.event.send` 是当前下行主链路
- BBClaw 音频应优先在 Gateway 内继续扩展
- BBClaw 设备应直接依赖一个新的官方 node media 通道

## 仍然有效的部分

以下原则仍然保留：

- BBClaw 仍然应被解释为硬件节点，而不是聊天渠道插件
- 与 OpenClaw 的长期关系仍以官方 `nodes` 路线为锚点
- 上游 PR 仍应优先选择通用、可解释、最小改动的能力
