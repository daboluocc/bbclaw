# 状态灯 PWM 亮度控制

## 现状分析

- 当前状态灯驱动使用纯 GPIO 高低电平输出，仅支持全亮 / 全灭
- 对于面包板上的 RGB 模块，夜间或近距离调试时亮度偏高
- 现有 `bb_led.c` 已经具备完整的状态机与闪烁节奏，缺的是底层占空比控制

## 方案设计

目标是在不改变现有灯语语义的前提下，引入统一的 PWM 亮度控制。

方案：

1. 保留现有 `BB_LED_*` 状态机和渲染逻辑
2. 将底层输出从 `gpio_set_level()` 切换为 `LEDC PWM`
3. 新增全局配置项：
   - `BBCLAW_STATUS_LED_BRIGHTNESS_PCT`
   - 默认用较低亮度，避免 RGB 模块过亮
4. 继续支持：
   - RGB 模块三通道输出
   - 独立 R/Y/G 三灯输出
   - `BBCLAW_STATUS_LED_GPIO_ON_LEVEL` 的高电平点亮 / 低电平点亮逻辑

## 改动范围

- `include/bb_config.h`
  - 增加状态灯 PWM 亮度配置
- `src/bb_led.c`
  - 初始化 LEDC timer + channel
  - 输出接口改为 duty 设置
- `docs/pin_map.md`
  - 更新状态灯说明，补充 PWM 亮度配置

## 验证计划

1. `make -C firmware build`
   - 确认 LEDC 初始化与驱动接口编译通过
2. 真机验证
   - `IDLE / RECORDING / PROCESSING / ERROR` 状态下灯语仍正确
   - 亮度明显低于改动前
   - 闪烁状态没有异常残影或反相

