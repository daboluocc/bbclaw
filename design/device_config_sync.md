# 设备配置同步机制设计

## 概述

实现 cloud → 设备的实时配置同步，支持云端动态控制设备功能开关（如蜜语功能）。

## 配置消息格式

### 1. Welcome 消息（设备连接时）

设备通过 WebSocket 连接到 cloud 后，cloud 在 welcome 消息中下发初始配置：

```json
{
  "type": "welcome",
  "deviceId": "BBClaw-0.4.1-C7EB89",
  "homeSiteId": "010cbb21-ece3-5953-91ae-e6702e6072b6",
  "config": {
    "version": 1,
    "updatedAt": "2026-04-30T18:00:00Z",
    "miyu_enabled": true,
    "volume_pct": 80,
    "speed_ratio_x10": 10,
    "speaker_enabled": true
  }
}
```

### 2. Config Update 消息（配置变更时）

云端配置变更后，通过 WebSocket 推送给在线设备：

```json
{
  "type": "config.update",
  "deviceId": "BBClaw-0.4.1-C7EB89",
  "config": {
    "version": 2,
    "updatedAt": "2026-04-30T18:10:00Z",
    "miyu_enabled": false
  }
}
```

**注意**：
- `config.update` 只包含变更的字段（增量更新）
- `version` 单调递增，用于检测配置冲突
- 设备收到后应用配置并持久化到 NVS

## 配置项定义

| 配置项 | 类型 | 说明 | 默认值 |
|--------|------|------|--------|
| `miyu_enabled` | bool | 蜜语功能开关 | `false` |
| `volume_pct` | int | 音量百分比 (0-100) | `80` |
| `speed_ratio_x10` | int | 播放速度 (10=1.0x, 12=1.2x) | `10` |
| `speaker_enabled` | bool | 扬声器开关 | `true` |

## 实现流程

### Cloud 端

1. **配置存储**：
   - 在 `router.Hub` 添加 `deviceConfig map[string]DeviceConfig`
   - 提供 `SetDeviceConfig(deviceID, config)` 和 `GetDeviceConfig(deviceID)` 方法

2. **配置下发**：
   - 设备连接时，在 welcome 消息中包含 `config` 字段
   - 配置更新时，调用 `hub.BroadcastToDevice(deviceID, configUpdateEnvelope)`

3. **配置更新 API**：
   - `POST /v1/devices/{deviceId}/config` - 更新设备配置
   - 请求体：`{"miyu_enabled": false}`
   - 响应：`{"ok": true, "version": 2}`

### 固件端

1. **配置接收**：
   - 在 WebSocket 消息处理中添加 `config.update` 类型
   - 解析配置并应用到运行时

2. **配置持久化**：
   - 使用 NVS 存储配置：namespace `bbclaw`, key `device/config`
   - 启动时加载配置，连接时与 cloud 同步

3. **配置应用**：
   - 提供 `bb_device_config_get(key)` 和 `bb_device_config_set(key, value)` API
   - 各模块通过 API 读取配置控制功能行为

## 配置冲突处理

- 设备本地配置版本 < cloud 版本：应用 cloud 配置
- 设备本地配置版本 > cloud 版本：忽略（可能是网络延迟导致的旧消息）
- 设备本地配置版本 = cloud 版本：跳过（已是最新）

## 蜜语功能实现

蜜语功能根据 `miyu_enabled` 配置控制：
- `true`：启用蜜语功能（具体实现待定）
- `false`：禁用蜜语功能

**注意**：蜜语的具体实现位置（设备端/adapter 端/cloud 端）需要进一步确认。
