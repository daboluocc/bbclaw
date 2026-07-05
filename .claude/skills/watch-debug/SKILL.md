---
name: watch-debug
description: "Use when debugging, testing, or driving the BBClaw 手表 (Waveshare ESP32-S3-Touch-AMOLED-2.06 watch board) — screenshots, key/PTT injection, serial logs, zero-button flash cycle, recorder-mode testing. Full self-closed loop: 改代码 → build → flash → 截图/日志验证, no human hands needed. Triggers: \"手表截图\", \"手表调试\", \"手表烧录\", \"watch screenshot\", \"手表日志\", \"录音测试\", \"手表按键\", or any watch-board firmware iteration. For the production bbclaw PCB use device-monitor instead."
---

# Watch Debug — 手表全自主调试闭环

手表 = Waveshare ESP32-S3-Touch-AMOLED-2.06 拓展板（ADR-040）。本 skill 固化
截图 / 按键注入 / 日志 / 零按键烧录的完整闭环与全部已知坑。生产 bbclaw PCB
用 device-monitor skill，不要混用（端口指纹不同）。

## 端口指纹（TinyUSB 双 CDC，ADR-015）

```bash
CDC0=/dev/cu.usbmodem1234561   # ESP_LOG 文本日志
CDC1=/dev/cu.usbmodem1234563   # devmon 二进制协议(截图/按键/重启)
# 通配: /dev/cu.usbmodem123456*  奇数尾=CDC0, 3 尾=CDC1
# ROM bootloader 态(烧录窗口)会变成别的名字(如 usbmodem212201),
# 用 `ls /dev/cu.usbmodem* | grep -vE '0010000001|123456'` 找
# 0010000001 是诱饵设备,永远别碰
```

## 截图（1.5s/帧）

```bash
SCRATCH=<scratchpad>
firmware/scripts/watch_screenshot.sh $SCRATCH/shot.png   # 自动找 CDC1
```
然后 Read 该 PNG。UI 迭代优先用模拟器零烧录出图：
`make sim-build-watch && ./simulator/build-watch/bbclaw_sim --export $SCRATCH/sim.png <mode>`

## 按键注入

```bash
python3 firmware/scripts/devmon_key.py --port $CDC1 {ok|back|up|down|left|right|ok-long}
```
- 手表触摸语义映射：tap 行=直接点按（LVGL 原生,注入不了）；注入的是 nav 事件
  （up/down=滚动, ok=确认/进设置, back=返回, left=聊天态进设置）
- **LOCKED（密语锁）下所有 nav 静默丢弃**——注入显示 `event=OK` 但无
  STATE_TRANSITION 就是锁着。解锁只能用户对设备说密语，让用户解锁再测。
- PTT 无注入通路（物理 BOOT 键/屏上圆钮）；录音书签测试需用户按键。

## 日志（禁止前台 make monitor——会卡死会话）

```bash
# 短抓(python serial 读 CDC0, timeout 秒级):
python3 -c "
import serial,time
s=serial.Serial('$CDC0',115200,timeout=0.5); t0=time.time(); out=[]
while time.time()-t0<10:
    l=s.readline().decode('utf-8','replace').strip()
    if l: out.append(l)
print('\n'.join(out[-40:]))"
```
- ⚠️ **boot 期 2.9–3.3s CDC0 有确定性日志丢失窗口**（寄存器 dump 打满 TX 环）。
  SD 挂载/audio init 的日志常被吞——**日志缺失≠代码没跑**，改用功能验证
  （截图看 UI 状态/设置行文案）。
- 长期监听用 Monitor 工具跑 python 捕获脚本（别用裸 bash 后台,会被 harness 杀）。

## 零按键烧录闭环（devmon reboot v3，已长时间验证）

```bash
FW=/Volumes/1TB/github/daboluocc-bbclaw/firmware
# 1) 构建(隔离目录,别抢共享 build/;绝对路径,后台 shell cwd 会重置):
. ~/esp/esp-idf/export.sh && idf.py -B $FW/build-ws206 \
  -DSDKCONFIG=$FW/build-ws206/sdkconfig \
  -DSDKCONFIG_DEFAULTS="sdkconfig.defaults;boards/waveshare-amoled-206/sdkconfig.board;sdkconfig.cloud" build
# 2) 软重启进 ROM bootloader(全自动,0 按键):
cd $FW/build-ws206 && python3 $FW/scripts/devmon_reboot.py --port $CDC1
# 3) 等新端口出现(≤30s)后烧:
port=$(ls /dev/cu.usbmodem* | grep -vE '0010000001|123456' | head -1)
python -m esptool --chip esp32s3 -p "$port" -b 460800 --before default_reset \
  --after hard_reset write_flash "@flash_args"
# 4) 设备自动重启,CDC 重新枚举(~10s),截图验证
```
- 烧录必须用 `sdkconfig.cloud` 链（用户要求 cloud_saas 模式）
- build 前 `ps aux | grep claude` 留意并行会话,别并发 build 同目录

## 录音模式测试（ADR-044 RECORDER 态）

- 入口：设置页「Recording」行**双击**确认（5s 窗口）；无卡显示 `Recording · no SD card`
- 录音页：呼吸红点+REC+时长+`N seg · M mark · SD x.xGB free`
- PTT 短按=书签；**右滑双按(3s 内)=停止**
- 数据在 SD：`/sdcard/ambient/<sessionId>/<seq>.opus` + `index.jsonl`
  （无法远程读 SD；验证靠日志 `bb_recorder: session end: N segments` + 录音页计数）
- LOCKED/空闲锁屏在 RECORDER 态豁免；退出后恢复

## 其它已知坑

- **libopus 栈账**:USE_ALLOCA 编译,一次 opus_encode 吃 ~24KB 调用任务栈——
  新任务用编码链必须 40KB 栈;PSRAM 栈溢出不 fault(死点飘忽+堆检测全绿),
  已开 FREERTOS_WATCHPOINT_END_OF_STACK
- **烧录端口扫描**:桌上若有其它 ESP 板(如 ESP32-C3 在 212201),`grep -vE`
  排除清单要加上它,否则 esptool 连错板子;手表 bootloader 端口名不固定
  (212201/212301/212401 都见过)
- 本板 **panic 输出不可达**(UART0 关+TinyUSB 占 USB):排障靠 RTC noinit
  面包屑 + boot report(开机 8s 上报 reset_reason+crumb,bb_recorder 有现成模式)

- FT3168 偶发 `I2C read error`：疑自动休眠 NACK，功能正常，别当 bug 修
- I2C 全家福基线：`0x18 ES8311 / 0x34 AXP2101 / 0x38 FT3168(偶尔漏) / 0x40 ES7210 / 0x51 PCF85063 / 0x6B QMI8658`
- mic 走 ES7210 不是 ES8311；喇叭无声先查 ES8311 时钟门控 readback 日志
- Edit 工具报 "File has been modified"→ 有并行会话在动同一文件，让路
- 硬件真相源：`design/boards/waveshare-esp32-s3-touch-amoled-2.06.md` + `firmware/boards/waveshare-amoled-206/board_config.h`
