# 点阵呼吸待机

## 现状分析

BBClaw 主板没有 IMU，设备的休眠唤醒源是导航按键、触摸（若板型支持）和云端消息。
现有息屏状态机在空闲到达 `DIMMING` 后仅降低当前页面亮度，进入 `SLEEPING` 时将
CO5300 AMOLED 亮度降为 0、发送 `DISPOFF`，并允许 ESP32-S3 自动 light sleep。

完全息屏功耗最低，但用户无法从屏幕判断设备仍在线。AMOLED 黑色像素几乎不发光，
适合用少量低亮度像素表达“在线待机”。

## 方案设计

- 复用现有 `DIMMING` 阶段作为环境待机，不增加新的电源状态。
- 环境待机显示纯黑全屏和居中的 `Z · · ·`，Z 与三个点错相缓慢呼吸。
- 面板亮度使用最低档 `BB_BRIGHTNESS_MIN`，动画以 200 ms 步进（5 FPS）刷新。
- BBClaw 生产板环境待机常驻；手动关屏仍会执行 `DISPOFF + light sleep`。
- ACTIVE、WAKING 和手动唤醒均关闭环境待机；按键、触摸及云端消息唤醒链路不变。
- BBClaw 无 IMU，保持 `BBCLAW_SLEEP_MANAGER_IMU_WAKE_ENABLED=0`。

## 并发约束

息屏状态机运行在 `stream_task`，不得直接调用 LVGL。状态机只写环境待机请求标志；
对象显隐与动画刷新由 LVGL timer 完成。动画退出后 timer 自行暂停，避免在真正休眠时
以 5 FPS 唤醒 CPU。

## 改动范围

- `firmware/src/bb_sleep_manager.c`：状态转换时请求启停环境待机，DIMMING 使用最低亮度。
- `firmware/src/bb_page_standby.c`：新增顶层三点呼吸覆盖层和 LVGL 定时器。
- `firmware/include/bb_page_standby.h`：新增线程安全的环境待机请求接口。

## 验证计划

1. `make -C firmware sim-build`：确认 LVGL API 与模拟器构建通过。
2. `make -C firmware build`：使用 ESP-IDF 构建 BBClaw 固件。
3. 真机观察：进入 DIMMING 后显示低亮度 `Z · · ·` 呼吸效果；超过原有 SLEEPING 超时后仍保持可见。
4. 真机唤醒：验证导航按键、触摸和云端消息均恢复正常页面。
5. 串口日志：确认 `ACTIVE → DIMMING` 状态转换；BBClaw 生产板不再自动进入 `SLEEPING`。
