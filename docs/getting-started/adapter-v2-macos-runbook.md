# Adapter V2 macOS 常驻运行手册

本文记录 BBClaw Adapter V2 在 macOS 上的生产式本地运行方式。目标是登录后自动启动、
异常退出后自动拉起、持续连接 BBClaw Cloud，并让语音会话在指定项目目录中运行。

## 1. 构建与安装

不要用 `make run` 做长期常驻。它是 Air 热重载开发入口，依赖当前终端且会在源码变化时重启。
常驻服务使用固定安装路径：

```bash
cd adapter
make install
~/.local/bin/bbclaw-adapter version
```

运行时配置以 `~/.bbclaw-adapter/settings.json` 为准；`.env` 只用于首次播种。

## 2. launchd 配置

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
        <string>/Users/USER/.local/bin/bbclaw-adapter</string>
    </array>
    <key>WorkingDirectory</key>
    <string>/ABS/PATH/TO/DEFAULT/PROJECT</string>
    <key>EnvironmentVariables</key>
    <dict>
        <key>PATH</key>
        <string>/Users/USER/.local/bin:/opt/homebrew/bin:/usr/local/bin:/usr/bin:/bin</string>
        <key>BBCLAW_DEFAULT_CWD</key>
        <string>/ABS/PATH/TO/DEFAULT/PROJECT</string>
        <key>BBCLAW_OPEN_ADMIN</key>
        <string>false</string>
    </dict>
    <key>RunAtLoad</key>
    <true/>
    <key>KeepAlive</key>
    <true/>
    <key>ThrottleInterval</key>
    <integer>5</integer>
    <key>StandardOutPath</key>
    <string>/Users/USER/.bbclaw-adapter/launchd.stdout.log</string>
    <key>StandardErrorPath</key>
    <string>/Users/USER/.bbclaw-adapter/launchd.stderr.log</string>
</dict>
</plist>
```

加载或更新：

```bash
plutil -lint ~/Library/LaunchAgents/com.bbclaw.adapter.plist
launchctl bootout "gui/$(id -u)/com.bbclaw.adapter" 2>/dev/null || true
launchctl bootstrap "gui/$(id -u)" ~/Library/LaunchAgents/com.bbclaw.adapter.plist
launchctl enable "gui/$(id -u)/com.bbclaw.adapter"
launchctl kickstart -k "gui/$(id -u)/com.bbclaw.adapter"
```

修改 plist 后必须 `bootout + bootstrap`，单纯 `kickstart` 不会可靠刷新环境变量。

## 3. 无界面工具权限

Adapter 的 Claude Code Driver 当前不实现交互式工具批准回传（`ToolApproval=false`）。
如果 Claude CLI 使用默认 `auto` 权限模式，Bash 被拦后，设备上说“确认批准”只会成为新一轮
普通文本，不能批准上一轮工具调用。

可信的个人设备需要无人值守执行工具时，在 `EnvironmentVariables` 中显式加入：

```xml
<key>AGENT_CLAUDE_CODE_EXTRA_ARGS</key>
<string>--dangerously-skip-permissions</string>
```

这会允许 Agent 直接执行命令，包括门禁、射频等物理动作。共享设备、公共空间或不可信账号
不应启用；这些场景应等待 Driver 实现真实的审批回传，而不是用语音文本模拟批准。

## 4. 验证

```bash
launchctl print "gui/$(id -u)/com.bbclaw.adapter"
curl -fsS http://127.0.0.1:18080/healthz
tail -f ~/.bbclaw-adapter/adapter-runtime.log
```

验收标准：

- launchd 状态为 `running`；
- `/healthz` 顶层 `status=ok`；
- `cloud.connected=true`；
- 设备按键后出现 `ptt.event device=...`；
- 说话后依次出现 `transcript_request_recv`、`agent_start`、`reply_delta_recv`；
- 工具动作出现 `tool_use` 后继续产生执行结果，而不是回复“需要批准”。

固定运行日志为 `~/.bbclaw-adapter/adapter-runtime.log`。串口只负责确认设备侧按键、录音和
WebSocket 上行；Claude 工具权限问题应优先查 Adapter 日志和实际 CLI 启动参数。

## 5. 升级

```bash
git pull --ff-only
make -C adapter install
launchctl kickstart -k "gui/$(id -u)/com.bbclaw.adapter"
curl -fsS http://127.0.0.1:18080/healthz
```

用 `bbclaw-adapter version` 核对安装二进制版本；用 SHA-256 对比
`~/.local/bin/bbclaw-adapter` 与 `adapter/bin/bbclaw-adapter`，避免“源码已更新但后台仍跑旧二进制”。
