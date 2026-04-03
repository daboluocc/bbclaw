# Changelog

All notable changes to this project will be documented in this file.

The format is based on [Keep a Changelog](https://keepachangelog.com/en/1.0.0/),
and this project adheres to [Semantic Versioning](https://semver.org/spec/v2.0.0.html).

## [0.1.0] - 2026-04-02

### Added
- 固件开源（Apache-2.0），ESP32-S3 + ES8311 + ST7789 全链路
- PTT 实时语音采集与上传
- 异步通知推送与轻量摘要展示
- WiFi 局域网连接模式
- HTTP 配对码流程
- LVGL 显示界面与 UI 资源
- Adapter / Cloud 运行面集成
- 本地 ASR 工具（FunASR）
- 架构文档、协议规范、硬件引脚与 BOM 文档

### Fixed
- 配对 HTTP 栈稳定性、TTS 采样率、配对码语音播报
- 设备码 JSON 排序稳定性、HTTP body 大小限制
- Makefile 生成目标整合、显示与文档清理
