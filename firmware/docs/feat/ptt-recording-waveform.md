# PTT 录音波纹检测显示

## 现状分析

- 当前固件主显示路径已经切到 `src/bb_lvgl_display.c`，`TX` 状态只显示 `LISTENING` 与“正在聆听...”
- 用户要判断麦克风是否真正收到有效输入，仍需依赖串口日志里的 VAD / audio diag 信息
- 旧的 `src/bb_display_bitmap.c` 中存在“假录音条”动画，但该实现不在当前编译路径中，且动画不反映真实 PCM 幅度

## 方案设计

目标是在 **PTT 按下** 且设备处于 `TX` 状态时，直接在 LVGL 上展示“录音是否有效”的实时反馈。

方案分两层：

1. **采集层**
   - 在本地录音链路读取 PCM 后，计算一个轻量级输入电平
   - 计算基于单帧 `mean_abs + peak_abs`，不引入 FFT，不增加传输链路复杂度
   - 结果只写入一个轻量共享状态，不直接跨线程操作 LVGL

2. **显示层**
   - `TX` 时在现有 active view 内切换到 speaking 子视图
   - speaking 视图包含：
     - 顶部状态栏（沿用现有）
     - 中部录音 badge 与扩散 halo
     - 底部条形波纹
     - 文案提示“已检测到声音 / 请靠近麦克风说话”
   - 条形波纹用 LVGL 原生 `lv_obj` 画短柱，不使用逐帧 SVG
   - 波纹总强度由真实输入电平驱动，单根柱子的起伏再叠加少量相位抖动，做出更自然的“波纹感”

## 改动范围

- `include/bb_display.h`
  - 增加录音电平写入接口
- `src/bb_lvgl_display.c`
  - 新增 speaking 子视图与波纹刷新 timer
  - `TX` 状态切换到 speaking 视图
- `src/bb_radio_app.c`
  - 在 arming / streaming 采样路径里计算实时录音电平
  - 在退出 `TX` 时清零录音电平状态

## 验证计划

1. `make -C firmware build`
   - 确认接口改动与 LVGL 布局代码编译通过
2. 真机验证
   - 按下 PTT 时屏幕切到 speaking 视图
   - 对着麦克风说话时波纹明显抬升，静音时保持低位
   - 松开 PTT 后 speaking 视图退出，恢复 `RX/RESULT/READY`
3. 观察串口
   - 确认没有新增 LVGL 线程竞争、WDT 或 ringbuffer 相关异常

