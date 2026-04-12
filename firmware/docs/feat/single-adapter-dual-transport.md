# Feature: 单 Adapter 双通道承载（local_home + cloud_saas）

> 状态：已实现（adapter 侧）
> 创建：2026-04-12

## 1. 目标

家里只运行一个 `bbclaw-adapter` 进程，同时支持两类设备：

- `local_home` 固件：设备直接访问本地 adapter HTTP
- `cloud_saas` 固件：设备先连 Cloud，Cloud 需要 Home Adapter 时由同一个 adapter 承接 relay

不再要求用户手动把 adapter 切成 `local` 或 `cloud` 二选一模式。

## 2. 结论

adapter 的职责本来就分成两块，而且两块并不冲突：

1. 本地 ingress：接收设备 HTTP 音频流，做 ASR/TTS/转发
2. Cloud relay：以 `home_adapter` 身份出站连接 Cloud，承接家庭侧回复链路

因此改成：

- 默认省略 `ADAPTER_MODE`，按 `auto` 处理
- 本地 HTTP 常驻
- 若存在 `CLOUD_WS_URL`，则自动同时开启 cloud relay

## 3. 行为

### 3.1 `ADAPTER_MODE=auto`

- 省略 `ADAPTER_MODE` 时等价于此模式
- 本地 HTTP 开启
- 如果配置了 `CLOUD_WS_URL`，cloud relay 开启
- 推荐生产默认值

### 3.2 `ADAPTER_MODE=local`

- 只开启本地 HTTP
- 用于局域网调试或纯本地部署

### 3.3 `ADAPTER_MODE=cloud`

- 只开启 cloud relay
- 仅保留给排障/调试

## 4. 健康检查

`GET /healthz` 返回聚合状态：

- `local.enabled` / `local.ready`
- `cloud.enabled`
- `cloud.connected`
- `cloud.lastError`

当本地 HTTP 可用但 cloud relay 暂时未连上时：

- HTTP 仍返回 `200`
- `status=degraded`

这样不会误伤 `local_home` 固件，但可以明确看出 `cloud_saas` 路径有问题。

## 5. 影响

- `local_home` 设备不再依赖 adapter 角色切换
- `cloud_saas` 设备不再因为 adapter 没切到 cloud 模式而出现 `HOME_ADAPTER_OFFLINE`
- 用户部署说明统一成“只启动一个 adapter”
