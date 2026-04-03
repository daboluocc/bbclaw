# BBClaw 使用手册

本手册帮助你从零开始配置 BBClaw 设备并投入使用。

---

## 准备工作

### 硬件清单

- ESP32-S3 开发板
- ES8311 音频编解码模块（麦克风 + 扬声器）
- 1.47″ ST7789 显示屏
- PTT 按键
- 振动马达（可选）
- USB 数据线

接线参考见 [firmware/docs/pin_map.md](../firmware/docs/pin_map.md)。

### 软件环境

- [ESP-IDF v5.5.2](https://docs.espressif.com/projects/esp-idf/en/v5.5.2/esp32s3/get-started/index.html)
- macOS / Linux / WSL
- 一台常驻运行的电脑（Mac mini / PC / 树莓派等），用于运行 Adapter

---

## 第一步：烧录固件

### 1.1 克隆仓库

```bash
git clone https://github.com/zhoushoujianwork/bbclaw.git
cd bbclaw
```

### 1.2 配置固件

通过 menuconfig 设置你的参数：

```bash
make -C firmware menuconfig
```

需要配置的关键项（在 `BBClaw Configuration` 菜单下）：

| 配置项 | 说明 |
|--------|------|
| **Transport Profile** | 选择 `local_home`（局域网）或 `cloud_saas`（公网） |
| **Wi-Fi SSID** | 你的 WiFi 名称 |
| **Wi-Fi Password** | 你的 WiFi 密码 |
| **Adapter Base URL** | Adapter 地址，如 `http://192.168.1.100:18080`（局域网模式） |
| **Adapter Auth Token** | Adapter 认证 token，需与 Adapter 配置一致 |

> 如果不在 menuconfig 中配置 WiFi，设备启动后会自动进入配网模式（热点名 `BBClaw-Setup-xxxx`，密码 `bbclaw1234`），连接后访问 `http://192.168.4.1` 输入 WiFi 信息。

### 1.3 编译并烧录

将设备通过 USB 连接电脑，然后：

```bash
make -C firmware build    # 编译
make -C firmware flash    # 烧录
make -C firmware monitor  # 查看日志
```

或一步到位：

```bash
make -C firmware all      # 编译 + 烧录 + 监控
```

看到日志中出现 `WiFi connected` 和 `Adapter health OK` 即表示固件运行正常。

---

## 第二步：安装 Adapter

Adapter 是 BBClaw 的运行面，负责接收设备音频并对接 AI 服务。我们在 GitHub Release 中提供各平台的预编译二进制文件。

### 2.1 下载

前往 [Releases](https://github.com/zhoushoujianwork/bbclaw/releases) 页面，根据你的平台下载对应版本：

| 平台 | 文件名 |
|------|--------|
| macOS (Apple Silicon) | `bbclaw-adapter-darwin-arm64` |
| macOS (Intel) | `bbclaw-adapter-darwin-amd64` |
| Linux (x86_64) | `bbclaw-adapter-linux-amd64` |
| Linux (ARM64) | `bbclaw-adapter-linux-arm64` |
| Windows | `bbclaw-adapter-windows-amd64.exe` |

每个平台提供两个版本：

- **internal** — 局域网版，设备与 Adapter 在同一网络内直连
- **internet** — 公网版，通过云端中转，支持设备在任意网络使用

### 2.2 配置

下载后赋予执行权限（macOS / Linux）：

```bash
chmod +x bbclaw-adapter-*
```

创建配置文件 `adapter.yaml`：

#### 局域网版（internal）

```yaml
listen: "0.0.0.0:18080"
auth_token: "your-secret-token"    # 与固件 menuconfig 中设置的一致

asr:
  provider: "openai_compatible"    # 或 "local"
  base_url: "http://127.0.0.1:18081"

openclaw:
  gateway_url: "ws://your-gateway:19089/gateway"
  session_key: "agent:main:bbclaw"
```

#### 公网版（internet）

```yaml
auth_token: "your-secret-token"

cloud:
  base_url: "https://cloud.bbclaw.cc"
  device_token: "your-device-token"  # 在 bbclaw.cc 注册后获取

asr:
  provider: "openai_compatible"
  base_url: "http://127.0.0.1:18081"

openclaw:
  gateway_url: "ws://your-gateway:19089/gateway"
  session_key: "agent:main:bbclaw"
```

### 2.3 启动

```bash
./bbclaw-adapter-darwin-arm64 -c adapter.yaml
```

看到 `Adapter ready` 即启动成功。

### 2.4 常驻运行

建议将 Adapter 设为开机自启：

#### macOS（launchd）

创建 `~/Library/LaunchAgents/com.bbclaw.adapter.plist`：

```xml
<?xml version="1.0" encoding="UTF-8"?>
<!DOCTYPE plist PUBLIC "-//Apple//DTD PLIST 1.0//EN" "http://www.apple.com/DTDs/PropertyList-1.0.dtd">
<plist version="1.0">
<dict>
    <key>Label</key>
    <string>com.bbclaw.adapter</string>
    <key>ProgramArguments</key>
    <array>
        <string>/path/to/bbclaw-adapter-darwin-arm64</string>
        <string>-c</string>
        <string>/path/to/adapter.yaml</string>
    </array>
    <key>RunAtLoad</key>
    <true/>
    <key>KeepAlive</key>
    <true/>
    <key>StandardOutPath</key>
    <string>/tmp/bbclaw-adapter.log</string>
    <key>StandardErrorPath</key>
    <string>/tmp/bbclaw-adapter.err</string>
</dict>
</plist>
```

```bash
launchctl load ~/Library/LaunchAgents/com.bbclaw.adapter.plist
```

#### Linux（systemd）

创建 `/etc/systemd/system/bbclaw-adapter.service`：

```ini
[Unit]
Description=BBClaw Adapter
After=network.target

[Service]
ExecStart=/path/to/bbclaw-adapter-linux-amd64 -c /path/to/adapter.yaml
Restart=always
RestartSec=5

[Install]
WantedBy=multi-user.target
```

```bash
sudo systemctl enable bbclaw-adapter
sudo systemctl start bbclaw-adapter
```

---

## 第三步：验证

1. 确认 Adapter 已启动并显示 `ready`
2. 设备上电，观察屏幕显示连接状态
3. 按住 PTT 按键说话，松开后等待 AI 回复
4. 屏幕上显示对话内容即表示全链路通畅

---

## 常见问题

### 设备连不上 WiFi

- 检查 SSID 和密码是否正确
- 设备仅支持 2.4GHz WiFi
- 可通过配网模式重新设置：长按 PTT 5 秒进入 SoftAP 模式

### 设备连不上 Adapter

- 确认设备和 Adapter 在同一网络（局域网模式）
- 检查 Adapter 地址和端口是否正确
- 检查 auth_token 是否一致
- 查看 Adapter 日志排查

### 公网模式无法连接

- 确认已在 bbclaw.cc 完成注册和设备绑定
- 确认 Adapter 使用的是 internet 版本
- 检查家中网络是否正常，Adapter 是否在运行
