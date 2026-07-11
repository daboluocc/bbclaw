# Changelog

All notable changes to this project will be documented in this file.

The format is based on [Keep a Changelog](https://keepachangelog.com/en/1.0.0/),
and this project adheres to [Semantic Versioning](https://semver.org/spec/v2.0.0.html).

## [Unreleased]

### Fixed
- **固件:息屏/变暗/抬手唤醒真正物理生效(手表,ADR-046 根因终案)**:用户反馈「一直
  没有息屏」。诊断发现 ADR-046 的息屏此前**从未物理生效**——只有软件状态机在变,面板
  一直全亮显示。根因两层:
  - **QSPI 命令编帧缺失(总根因)**:SH8601/CO5300 走 QSPI 时命令必须编成
    `(0x02<<24)|(cmd<<8)`。init 命令经驱动内部 `tx_param` 自动编帧所以能点亮;但运行期
    亮度/息屏直接调 `esp_lcd_panel_io_tx_param` 传的是**裸命令**(0x51/0x28…),面板一律
    忽略(事务仍返回 ESP_OK,骗过日志)。→ 亮度调节、变暗、息屏命令全部无效。已给所有
    运行期面板命令补上 `CO5300_QSPI_CMD()` 编帧。
  - **息屏需 DISPOFF+SLPIN,非仅亮度 0**:0x51 写亮度 0 熄不灭 AMOLED(仍扫描发光)。
    进 SLEEPING 发 `DISPOFF(0x28)+SLPIN(0x10)` 真正下电熄屏,唤醒发 `SLPOUT(0x11)+~120ms
    稳定+DISPON(0x29)`(序列参考 Arduino_GFX CO5300 driver)。
  - **IMU 抬手唤醒阈值单位错**:阈值 1500 当 mg 用,但 QMI8658 样本是 m/s²(静止≈9.81,
    ±8g 满量程仅 78.5),`a_total>1500`(≈153g)物理永不触发 → 抬手唤醒长期失效。改为
    偏离重力基线 `|a_total-9.81|>2.5` m/s²。
  - 真机验收(手表):变暗可见、息屏真黑屏、抬手/触摸/按键唤醒全过。
- **固件:adapter 每次打断/朗读泄漏 ~16KB PSRAM(手表)**:`turn_cancel_task` /
  `notif_tts_task` 用 `xTaskCreateWithCaps` 创建却用普通 `vTaskDelete(NULL)` 自删,caps 的
  PSRAM 栈+TCB 不回收。改 `vTaskDeleteWithCaps(NULL)`(与 session 任务一致)。
- **固件:消息/提醒到达唤醒息屏(手表,ADR-046 §5)**:`on_message_arrived` 通路此前无
  调用点(死代码),息屏时来通知不亮屏。在 WS `session.notification` handler 挂 wake。

### Added
- **固件:息屏时间可在设置页调节(手表)**:此前 2min 变暗 / 3min 息屏是硬编码
  (板配置的 `BBCLAW_SLEEP_MANAGER_*` 宏其实未被使用,真实值来自 sleep_manager
  默认常量)。设置页新增「Sleep」行,点击循环 **Never / 30s / 1 min / 3 min / 5 min**,
  即时生效(变暗时间自动取息屏时间 2/3;Never=禁息屏状态机屏常亮),预设存 NVS
  (`bb_power_mgmt`,写走内部栈 commit 任务避 flash-cache-freeze),开机加载应用。
  真机验证:循环切换标签正确、每档 `sleep preset -> …` 应用 + `commit sleep preset`
  落盘、零 panic。
- **固件:录音回放改为「会话连续播放」(手表)**:录音磁盘仍按 60s 分段(断网重传
  粒度小),但浏览列表改为**按会话列出 + 显示总时长**,点会话**跨段无缝连续播放**
  (`bb_recplay_toggle_session` 遍历 000001.opus… 逐段解码接续);段级碎片下钻废除。
  真机验证:1:51 会话 seg1(60s)放完自动接 seg2(51s)连续播放,列表总时长正确。

### Changed
- **固件:SD 插卡检测提速——热插拔轮询挪到独立任务(手表)**:用户反馈插卡反应
  迟钝。根因:板上无 CD 引脚,热插拔靠软件轮询,而轮询内联在 stream_task 主循环、
  10s 一轮(怕阻塞按键才设这么慢),插卡最坏等 10s + 挂载(SDMMC 卡初始化)几百 ms。
  拔卡感觉即时是因为正在用 SD 的任务(录音/回放)会即时 I/O 失败,插卡却只有这一条
  慢通道。改为**独立低优先级任务 `sd_hotplug_task`(2s 一轮)**:挂载尝试即使阻塞也
  不卡主循环 UI/按键,插卡手感跟手(真机验证 `SD card hot-inserted and mounted` 秒级触发)。
  - 配套:`bb_sdcard` 挂载/卸载/CMD13 探测/格式化/selftest 全部**加锁串行化**
    (`s_card` 生命周期互斥)——轮询移出主循环后会与 UI 上下文的 mount/format 并发,
    不加锁会双挂载或 use-after-unmount;`mounted()` 仍无锁快读(advisory)。

### Added
- **固件:ambient 录音音质优化——双麦合成 + 向上式 AGC(手表)**:诊断用户拉回的
  三段真机录音发现两个结构性问题:①手表 ES7210 四通道 ADC 双麦(MIC1+MIC2,各 30dB)
  硬件在,但采集降混沿用 INMP441 单麦的「逐帧挑响一路、丢另一路」逻辑 → 双麦白费;
  ②录音电平严重偏低且忽大忽小(段间 RMS −41~−59dBFS,只用 11-12/16 bit),因增益
  为「30cm 近距离 PTT」标定,而 ambient 录几米外环境音天然低 20-30dB,且无 AGC。
  - **双麦均值**(`bb_audio_set_recorder_mix`,仅录音态):MIC1/MIC2 同场同源两路平均
    → 不相干噪声 −3dB(SNR +3dB),并消除逐帧挑麦跳变的瑕疵;对话/PTT 态不启用
    (近场常一只麦更近,保留挑响)。真机确认两路都活且平衡(energy_l≈energy_r)。
  - **向上式 AGC**(`bb_recorder` 内,双麦均值后 / Opus 编码前):只抬安静段、不动
    已够响的瞬态(零新增削顶),噪声门防静音泵噪,软限幅兜底,整会话持有状态跨段连续。
    参数在用户真实录音离线原型 + ffprobe/astats 上调定验证(000229 RMS −50→−29dB、
    峰值 −23→−5dB;000230 峰值 −0.4dB 原样保留、零硬削顶;229 零限幅命中)。
  - **真机验收(2026-07-09,手表)**:烧录启动干净;PWR 一键启录→双麦 mix ON→3 段
    连续(2×60s + 1×28s,均 EOS 收尾)→PWR 停录→mix OFF 恢复 PTT 语义;`session end:
    3 segments last_err=0`,recorder 栈 hwm 全程稳(48KB 余 ~25KB,AGC 浮点零压力),
    零 panic。**输出绝对电平待用户拔卡 ffprobe 复核**(算法已离线证明)。
- **固件:息屏/功耗管理启用(ADR-046,手表)**:静置 2min 变暗→3min 息屏(AMOLED
  亮度 0=近零功耗);按键/触摸唤醒;IMU 抬手唤醒(WAKING 2s 去抖,无操作回睡,
  有操作转常亮);录音/语音回合豁免。曾因随机崩被禁,本次根治四个结构性问题:
  ①100ms FreeRTOS 定时器回调在 Tmr Svc 小栈跑亮度渐变+任务击杀 → 废除定时器,
  状态机改 stream_task 内 tick(100ms 节流),IMU/触摸/消息事件旗标化单上下文消费;
  ②亮度渐变任务被外部 vTaskDelete 击杀(可能正在 QSPI 写) → 代数化优雅退出;
  ③CO5300 亮度命令与 LVGL 刷屏抢 QSPI 总线 → lvgl_port_lock 互斥;
  ④时间戳无符号下溢(motion 唤醒 1ms 被打回) → 带符号比较。
  真机验收:变暗/息屏时序精确×2 轮、三种唤醒路径全过、零 panic。
  devmon 新增注入:201=IMU 运动模拟、202=强制进配网(复现网络恢复收页,已验证
  现固件收页链路完好)。
- **固件+云端:ambient 回网补传(ADR-044 P1b)**:
  - 固件新模块 `bb_ambient_sync`:后台任务(16KB PSRAM 栈,优先级 4)在
    cloud_saas+WiFi+录音不活跃+语音链路空闲时,扫 `/sdcard/ambient/*/index.jsonl`
    把未上传段按序补传云端(`ambient.segment.start` → 文件字节二进制帧 →
    `segment.finish` → 等云端持久化 ack)。进度记每会话 `sync.state`
    (不改写 recorder 的 append-only 索引,单写者纪律);掉电/掉线最多重传一段
    (云端 O_TRUNC 幂等)。书签(`ambient.bookmark`)与会话收尾(`session.stop`)一并同步。
  - SD 低水位回收:剩余 <256MB 时从最旧「已全部上传且已收尾」会话开始整目录回收。
  - `bb_adapter_client` 新原语:`send_bin` 导出、ambient ack 等待槽
    (arm→finish→wait,照 sites.* 模式);`bb_radio_app_voice_busy()` 让路查询。
  - 云端(bbclaw-reference d723118):`handleDeviceWS` ambient.* 全套接收,段字节
    边收边落盘 `data/ambient/<deviceId>/<date>/`(不进内存/不走 turn 管线),
    fsync 后回 `ambient.segment.ack`;与 voice.stream 双向互斥;30 天生命周期
    (按录制日期过期);portal `GET /v1/ambient/segments` + `DELETE /v1/ambient/data`
    (归属校验,PIPL 自助删除)。**部署顺序:云端先于固件**(旧云端会把 ambient.*
    kind 转发给 home adapter 报错,补传会一直重试不丢数据,但别长期跨版本跑)。
  - **手表切 wss/https**(ADR-044 §5.3 环境音不明文):`sdkconfig.cloud` base URL
    切 `https://`,WS 升 wss + esp_crt_bundle,真机验证长连接稳定。
    网关零改动(https/wss 本就就绪);`sdkconfig.bbclaw.latest`(正式板)暂未切,
    待该板真机验证内存/连接后再切。已知代价:display_pull 1.5s 轮询现在每次
    全新 TLS 握手(~1 次/秒),后续优化方向=display 任务改走常驻 WS(ADR-042)。
  - **端到端验收 PASS(2026-07-07 真机)**:PWR 开录→3 整段+尾段→停录 20s 内全部
    补传 ack→云端落盘字节逐一吻合+journal 齐全+session.stop 收尾。
  - 验收路上修掉两个真机坑:①SD 无文件系统卡(新卡出厂 exFAT→FATFS err 13
    无出路)——录音入口格式化挂载自愈 bb_sdcard_mount_format();②**https 切换后
    TLS 握手栈溢出重启循环**——hist_fetch 等 4-8KB 明文时代任务栈扛不住 TLS
    (~8-10KB),进聊天页即 panic;全部 HTTPS 瞬态任务升 16KB PSRAM,顺带修
    agent_task WithCaps/vTaskDelete 不配对的每回合 PSRAM 泄漏。
    教训:**切 https 必须重审所有在调用方任务栈跑 TLS 的栈账**。
  - **录音段 .opus Ogg 完整性(批 ASR 前置,已核实+加固)**:此前 ffprobe 报 EOF
    的根因是 `*out_len=0` 覆写 bug 让段首 OpusHead/OpusTags 丢失、文件从数据页起头
    (非 BOS),已在「录音丢 3/4 音频终案」修掉——现固件每段重建编码器、段首正确写
    头,ffprobe/ffmpeg 干净读取解码。补齐剩余的规范缺陷:**段尾页现置 EOS 标志
    (0x04,RFC 3533/RFC 7845)**——`bb_ogg_opus_encoder_flush` 无论收尾于整帧边界
    (60s 段最常见,`pending==0`)还是帧中,都补齐一帧并在末页打 EOS,标记流终止 +
    末包 granule 截断;缺 EOS 的流 ffmpeg 能读但严格校验的批 ASR 服务会判为截断。
    host 端用同一 Ogg 封装逻辑 + 真实 libopus + ffprobe 验证三种收尾形态(帧中/
    整帧边界/空段)全部规范合规(首页 BOS、尾页 EOS、无杂散中途 EOS、ffprobe 零错)。
    设备侧解码/回放不受影响(decoder 忽略 header_type)。真机 ffprobe 待下次接板复核。
- **固件:Waveshare ESP32-S3-Touch-AMOLED-2.06 拓展板支持(ADR-040 第一阶段)**:
  - 新板 `waveshare-amoled-206`:2.06" QSPI AMOLED 410×502(CO5300,SH8601 兼容驱动)+
    ES8311 codec(既有代码路径首次真机启用)+ BOOT 键 PTT。触摸/AXP2101 电量/IMU/RTC/SD
    为后续阶段。
  - `bb_panel` 新增第三种显示总线 `BBCLAW_DISPLAY_BUS_QSPI`;LVGL 侧支持 2px 刷新对齐
    rounder(CO5300 硬性要求)与逐板缓冲配置(行数/PSRAM/单双缓冲)。
  - `make set-board` 现在会把 `boards/<name>/sdkconfig.board` 覆盖进 `sdkconfig`,
    OCT/QUAD PSRAM、flash 大小等逐板差异随切板一次切齐(此前要手动改)。
  - 设计文档:`design/boards/` 收录 AMOLED-2.06 与 LCD-1.85(第二块,待适配)硬件参考。
  - **真机点亮后修复两个上板即踩的坑**:
    - LVGL 缓冲不能放 PSRAM:esp_lcd SPI 对非 DMA 缓冲每次刷屏 malloc 同尺寸内部
      反弹缓冲,softAP 起来后内部 DMA largest(40960) < flush 块(41000) → flush 失败
      → LVGL 持锁整机冻结(UI/devmon/日志全死)。改内部 DMA 单缓冲 40 行常驻。
    - OTA platform 板级化(`BBCLAW_OTA_PLATFORM`):拓展板报 `esp32s3-ws-amoled-206`,
      云端按 platform 精确匹配 active → 不会把 bbclaw 正式板固件推给引脚完全不同的
      拓展板(否则 cloud 模式一联网就被 OTA 成黑屏)。既有板默认 `esp32s3` 不变。
  - 新增 `firmware/sdkconfig.cloud` 通用 overlay:任意板叠加即得 cloud_saas 构建。
  - **手表音频全链路打通**(PTT→双 mic→Opus→云 ASR→回复→TTS):
    - 喇叭:ES8311 DAC OSR 0x20→0x10(16kHz 正确系数)+ REG01 时钟门控须在
      MCLK 运行后写入 + 官方三步复位序。
    - 麦克风:原理图证实 MIC1/MIC2 挂独立 **ES7210**(0x40),非 ES8311——新增
      ES7210 最小驱动(I2S slave 16bit/30dB/双 mic),AEC 回环通道预留。
    - AXP2101 最小初始化(ALDO1 MIC 电轨/充电 200mA@4.1V/长按 4s 关机/关 TS)。
  - **devmon 手表适配三修**:worker 栈 4K→12K PSRAM(大屏 lv_snapshot 栈溢出秒崩);
    REQ_REBOOT 走 usb_persist+ROM 复位(esp_restart 在 WiFi 活跃时挂死,芯片复位
    但 USB 假死曾被误判"整机冻结");CDC 写主机不拉数据时限时放弃(原无限自旋)。
  - 新增 `scripts/watch_screenshot.sh` 手表拉屏脚本(410×502 一帧 1.5s,AI 迭代 UI 用)。
  - **手表触屏**(ADR-040 §UI.5):FT3168 手势→导航事件注入(tap=OK/上下滑/长按右滑=BACK),
    UI 层零改动全页面可触摸。
  - **手表竖屏 UI 适配**(ADR-040 §UI,设计文档先行):
    - 圆角安全区机制 `bb_ui_layout.h`:板级 CORNER_RADIUS(实测 R9.2mm=114px,
      产品尺寸图正面窗口=显示 AA 一比一)→0.293r 内缩两档安全区;顶栏/PTT 区
      占用圆角带、内容区居中(方屏板行为不变)。
    - 全部页面竖屏重构:待机时钟 hero 1.8x/配网/锁屏/OTA/开机/选择页居中构图,
      设置页/任务列表/选择器 64px 触摸行高,聊天 transcript 接内容盒上下铺满。
    - 模拟器支持手表分辨率(`make sim-build-watch`,零烧录 PNG 出图迭代)。
  - **手表触摸交互体系**(原生 LVGL 指针 indev,替换初版手势模拟层):
    - 跟手滚动/行点按执行/音量条拖动(1% 粒度)/右滑返回/左滑进设置
      (回复中可进,TTS 不断)/待机点击唤醒。
    - 悬浮 PTT 圆钮:聊天页专属(主题托管生命周期,overlay 天然盖住),
      按住说话,平时幽灵态按下点亮;物理 BOOT 键保留兜底。
  - **手表电量+充电展示**:bb_power 新增 AXP2101 硬件电量计后端(0xA4 SoC+
    充电方向+电池在位,I2C 懒加载);待机充电动效(电量→满格循环注入);
    顶栏电量+闪电图标数据链路激活。
  - **随身长录音 P1a(ADR-044,真机验收 PASS)**:SD 本地优先录音全链路——
    SDMMC 1-bit 挂载+写自检门禁+自动格式化恢复+热插卡轮询提醒(toast 弹窗);
    RECORDER 第四态(设置行双击/PWR 键一键启停,PTT=书签,双右滑停止,常显
    红点指示);60s Opus 分段落 SD+append-only 索引;周期 fsync+devmon 重启
    前停录。**排障沉淀**:libopus USE_ALLOCA 一次编码吃 ~24KB 任务栈(调用
    任务必须 ≥40KB 栈);SD 引脚与 UART0 冲突(手表板关 UART0 控制台);
    FATFS LFN;编码器 append 分片喂入(修"整段只录 60ms"隐蔽 bug)。
  - **IMU 分支合并**:QMI8658 驱动+息屏管理+CO5300 亮度(feature/imu-sleep-
    management 收编+编译修复,双板验证)。
- **固件+adapter_v2:长回合等待改「活动驱动空闲超时」——根治「AI 深思考被设备提前判超时」**:
  两端等待循环原是固定 deadline(90s→5min 只是止痛),事件到达不续期——回复
  生成好时设备已放弃、迟到帧被静默丢弃。现改为:回合内任何流式帧(delta/
  tool_call/status/tts/prompt,含被单调门丢弃的非单调快照)都刷新活动时间,
  **静默超 5 分钟才超时**(长 Bash 工具期间 adapter 层零事件下发,不能更小),
  30 分钟绝对兜底;多步长回合任意总时长存活。云端 idle 120s 滑窗有心跳续期
  无需改;云端 CLOUD_REPLY_WAIT_SECONDS=600s 固定总上限是 >10min 回合的
  最终瓶颈(后端 env 可热改,待定)。

### Changed
- **固件:设置显示优化——开关状态文案 + 会话显示标题而非 ID**:
  - **开关状态**:Miyu 行不再用裸 `on`/`off`,改 `Enabled` / `Disabled`(后续新增开关共用
    `toggle_state_label()`)。
  - **会话标题**:Sessions 选择器与主菜单 Sessions 行原来在无标题时直接显示 8 位十六进制
    session id,很难认。改为「标题(adapter 对首条 prompt 的自动摘要)→ 工作区名 cwd_name →
    短 id」的回退链,尽量显示人类可读名(`session_display_label()`)。
  - 固件 `bb_agent_session_info_t.title` 缓冲 24→64 字节:adapter 标题最长约 48B,旧 24B 会把
    中文标题截到 ~7 字。
  - 注:标题为空仍会退化到 id——adapter 仅在会话「跑完第一回合」后才自动生成标题(AutoTitle),
    全新空会话本就没有标题;此时优先显示工作区名。会话列表过多时选择器已可滚动(本轮早前已加)。
- **固件:设置菜单重构——固件信息收进「System Info」只读页 + 独立「Check Update」+ Sessions 行显示当前会话**:
  - 原「Firmware: <版本>」一行身兼「显示版本」与「点击 OTA」两职,现拆开:
  - **System Info(关于)**:新增只读子页(点进去看),列固件版本 / 设备 ID / 连接模式 / 空闲堆。
    任何模式都可进(版本信息在哪都有用)。
  - **Check Update(版本检测)**:独立菜单项(仅 cloud_saas,OTA 本就云端专属)。点击跑
    `bb_ota_check`,行内显示 `· checking… / · up to date / · check failed`;查到新版本弹原确认页选择是否升级。
  - **Sessions 行显示当前会话**:原来只是静态「Sessions」,现显示 `Sessions: <当前会话标题>`(过长
    marquee 滚动),靠进设置时预拉的 session 列表把活动会话 id 解析成人类标题,回退短 id / (none)——
    与 Adapter 行同套路。
  - 顺手修掉主菜单首次打开光标落点错位(`sel` 本应是可见行索引,旧代码误置成逻辑行 id)。
- **固件+云端:OTA 判断全部迁到后台——dev/dirty 放行 + #179 防砖护栏迁 cloud**:
  - **dev/dirty 也能 OTA**:删掉固件侧「dev/dirty 构建跳过 OTA」护栏(`bb_ota_is_dev_build`),
    是否升级一律由云端 `GET /v1/ota/check` 决定。原则:OTA 判断只放后台(后台可热改,固件改不动)。
  - **#179 防砖死循环护栏迁后台**:原固件在 NVS 里比 `last_try` 本地决定是否退避;现改为固件把
    `last_try`(上次**真正烧录并重启**的目标版本,仅 `bb_ota_apply_update` 置位,decline/下载失败永不
    置位)作为**事实**带进 check 请求 `&last_try=`,由云端 `loopGuardSuppress`(`last_try==offered &&
    current!=offered`)**决策**退避。固件侧不再做这个判断。菜单「Check Update」(跑 PSRAM 栈、不能读
    NVS)不带 last_try → 云端对手动路径不护栏(与旧固件菜单绕过语义一致)。
  - ⚠️ **部署顺序必须「先云端、后固件」**:否则窗口期固件本地护栏已删、云端护栏未上,撞上「版本号
    没递增」的坏发布会复活 #179 无限重刷。
  - ⚠️ **救砖只能 bump 版本号**:护栏只比版本串,同号重传(即便换了修好的二进制)会被持续退避;
    救砖发版必须用更高的新版本号(本就符合 one-tag-one-release)。

### Removed
- **固件:密语锁屏页移除底部状态栏**:锁屏页(`设备已锁定` / 密语解锁)底栏左下角的
  `[B] <cwd>`(butler 工作目录)与右下角的 `mem: N+M`(记忆条数)是开发/butler 调试信息,
  对被锁定的用户毫无意义。整条底栏(含分隔线 + 两个角标 + `bb_page_locked_update_footer`
  及其在 bb_lvgl_display 的刷新调用)一并移除,锁屏更干净。
- **固件:移除「声音(Voice/TTS)开关」——改由物理开关控制**:设备已有物理开关控制声音,
  软件层的 TTS 开/关因此冗余,全部移除:① 设置主菜单去掉 `Voice: on/off` 行;② 后台持久化
  (NVS `agent/tts`)、`bb_ui_settings_tts_enabled()` / `bb_ui_settings_preload_nvs()` 公共 API、
  `bb_radio_app` 的 preload 调用全删;③ chat 侧 `s_chat.tts_enabled` 及其门(回合结束 `will_speak`、
  流式断句起播、`tts_kick_or_spawn`)改为**常开**——固件总是合成/请求 TTS,静音交给物理开关;
  ④ 主题顶栏的 `T+/T-` 指示一并去掉(buddy-ascii / text-only,均为已退役主题)。
  注意:TTS 现在恒定合成(即便物理开关静音),若云端 TTS 按量计费,静音时仍会产生合成开销。

### Fixed
- **固件+adapter_v2:语音回复等待上限 90s→5 分钟——修「AI 想久了设备先判超时,回复真正生成好也收不到」**:
  cloud_saas 语音turn 的等待上限此前两侧都硬编码 90s(固件 `BBCLAW_HTTP_STREAM_FINISH_TIMEOUT_MS`
  本地 `xEventGroupWaitBits` 超时 + adapter_v2 `cloudrelay.ReplyWait`),而云端自身的
  `CLOUD_REPLY_WAIT_SECONDS` 早已是 600s。AI 多步思考/工具调用经常超过 90s:期间 adapter 的 15s
  心跳一直在保云端的 idle 计时器活着,但 90s 一到 adapter 自己的等待循环先放弃,把
  `voice.reply{text:"",replyWaitTimedOut:true}` 发给云端并 `cb.ev.end()` 关掉事件观察者——固件也在
  90s 处独立判超时挂断。此后 CLI 才吐出的真实回复因观察者已 `end()` 被静默丢弃,设备再也收不到。
  两处都改为 5 分钟(仍低于云端 600s 上限),不再是这条链路里最短的那一环。
- **adapter(claude-code 驱动):tool 调用按 tool_use.id 去重——修「问候却重播上一轮工具调用」**:
  设备只说了句问候,屏幕却把上一轮(设音量 / 门禁控制)的灰色工具行又画了一遍。根因在 agent 流:
  claude `--resume` 续接某些情况下会把上一轮的 `tool_use` 块重新吐进 stream-json,驱动逐条转成
  `EvToolCall` → 设备重画陈旧工具行(日志特征:tool_call 出现在本轮 ASR 结果**之前**)。驱动现按
  `tool_use.id`(claude 每次调用唯一且稳定)在**整条会话**内去重,重复出现的直接丢弃
  (`tool_use dup-skip` 日志),保证每个工具调用最多上屏一次。这是驱动层根治;若更新 adapter 后
  cloud_saas 仍重播,则是云端 relay 自身缓冲重放(私有仓,另查)。:本地 `make build`/`make flash` 烧的固件
  之前 esp_app_desc.version 来自 gitignore 掉的陈旧 `version.txt`(或落后的最新 tag),自报
  版本恒低于云端 active → 每次开机 OTA 检查都判「有更新」,弹升级框甚至把刚烧的调试固件
  OTA 覆盖掉。两处修复:
  1. **Makefile `stamp-version`**:`make build` 前用 `git describe --tags --dirty` 把版本写进
     `version.txt`(形如 `v0.5.17-69-g7544427-dirty`——tag 前缀 + 落后 tag 提交数 + 脏标记),
     本地构建总是自报真实 git 状态。发布路线(`release_local.sh` / CI `release.yml`)显式传
     `FW_VERSION=vX.Y.Z` 钉死发布版本,行为不变。
  2. **`bb_ota.c` dev 构建护栏**:`bb_ota_check()` 检测到运行版本是 git describe 的 dev/脏构建
     (含 `-dirty` 或 `-<N>-g<hash>` 提交数段)就直接短路、连云端都不打,`has_update=false`。
     干净 tag 发布与 release_local 推送(`vM.m.p-g<hash>`,无提交数段)不受影响,仍正常 OTA。
- **固件:设置菜单过长溢出 → 改为可滚动列表**:新增 Firmware 行后,cloud_saas 下主菜单已有
  6 行(Adapter / Sessions / Volume / Voice / Miyu / Firmware),`22(头) + 6×26 = 178px` 超过 bbclaw
  面板的 172px 高,尾部 Miyu/Firmware 行连同底部「Hold to exit」提醒一起被挤出屏幕、选不中也看不见。
  现在 `rows_box` 限高到「头部与底栏提醒之间」的可用区(`DISP_H - HEADER_H - FOOTER_H`)并开启纵向滚动
  (`LV_DIR_VER`,编码器驱动);`highlight_selected()` 在每次选中变化时 `lv_obj_scroll_to_view`
  把光标行滚入可视区,底栏提醒始终留白不被覆盖。右侧带一条 `MODE_AUTO` 的细 teal accent 滚动条
  (仅在内容溢出时出现,thumb 的位置/长度指示上下还剩多少),与选中行高亮同色,贴合点阵设计语言。
  对所有列表层级生效(主菜单 / Driver / Adapter / Sessions / Model 选择器,Sessions 最多 16 行尤其受益)。
- **固件:语音「不跟手」+ 头被吃掉 + 自己打断自己(三连)**——PTT 遥测日志把问题照清楚后定位:
  1. **头被吃掉**:VAD arm(攒够 240ms 说话才起流)触发那刻,arm 窗口里录的音**只 seed 最后一帧
     (~32ms)**,开头被丢 →「调到20」识别成「到20」。改为 arm 期间把每帧滚动累积进 8192B(~256ms)
     pre-roll,起流时整段开头一起 seed(仍静态缓冲不碰环,无 INT WDT 风险)。
  2. **松手后自己打断自己**:松手到 cloud ASR 返回有数秒,期间用户以为没生效再按 PTT,而 `cloud_wait`
     还在 → 判成 barge_in **把自己刚发出的正确那轮 turn.cancel 掉**。新增 barge-in 宽限
     (`BBCLAW_PTT_BARGE_IN_GRACE_MS`=2500):cloud-wait 开始后这段时间内、且 TTS 未开口时,PTT-down 视为
     「急着重按」不取消在飞轮;TTS 一旦开口则打断照常即时生效(ADR-028)。
  3. **反馈太弱**:ASR 等待期只有 buddy「thinking…」太隐晦。topbar 期间显式显示「识别中..」,回复落地时
     由 SESSION/state 帧自动还原会话 id。
- **固件:PTT 遥测改为观测专用**——`ptt_report_task` 不再从遥测路径调 `ws_client_ensure_connected()`
  (可能取锁/触发最长 5s 连接,理论上与在飞语音流争用);WS 未连接就直接丢弃该遥测事件。

### Added
- **固件:设置里新增「Firmware」行——看版本 + 点击触发 OTA 升级**:设置主菜单底部多一行
  `Firmware: <当前版本>`(任何模式都显示,方便核对设备版本)。在 cloud_saas 下点击 → 后台
  跑 `bb_ota_check`(PSRAM 栈任务,行内显示 `· checking…`):查到新版本就弹出原有 OTA 确认页
  (再按 OK 即开始下载/烧写/重启——确认页在 `lv_layer_top`,盖在设置层上且优先吃 OK/BACK,
  无需先退设置);已是最新显示 `· up to date`;失败显示 `· check failed`。复用启动自检那条
  确认→预热内部 RAM apply 任务的链路,不重复造轮子。local_home 无 OTA,点击显示 `· cloud only`。
- **固件 + adapter:PTT 按键边沿全量上报(`ptt.event` / `POST /v1/agent/ptt`)**:此前 adapter 只能
  间接感知 PTT——只有走完 VAD+ASR 出了 `voice.transcript`、或打断时发了 `turn.cancel` 才知道;空按、
  按住没说话、railed mic 没出声、以及任何松开,服务端全程无感知。现在固件在 `on_ptt_changed` 的每个
  边沿都上报一条带「识别事件类型」的轻量遥测:`down` 边按当时上下文分类为 `listen`(空闲按下→准备录音)/
  `barge_in`(回合在飞→打断)/`settings_exit`(设置覆盖层时按下);`up` 边为 `release`。cloud_saas 走
  device WS `kind:"ptt.event"`(云端 `RouteFromPeer` 对 device→adapter 的 request 通用透传,无需改云端),
  LAN 走 `POST /v1/agent/ptt`。固件侧单个常驻 worker 排空队列(快速 down/up 不丢、且不为每个边沿建任务,
  规避内部 RAM 碎片致建任务失败);adapter 侧纯日志 + `ptt_event` 指标,不触碰 agent 回合。日志:固件
  `phase=ptt_event_sent edge=.. action=.. seq=..`,adapter `cloudrelay: ⮟ ptt device=.. edge=.. action=.. seq=..`。
  两套 adapter 都加了 handler:`adapter/`(homeadapter + httpapi)与实际在跑的 `adapter_v2/`(cloudrelay)。
- **adapter_v2:设备上线本地日志**:relay 多路复用所有设备到一条云 WS,本地无单设备连接信号;现按流量推断
  presence——某设备首次发请求、或静默超 90s 后再次出现时,打 `cloudrelay: ● device online device=..`,
  让 adapter 日志能记录设备上线。

### Fixed
- **固件:后台/云端调音量导致设备重启(cache-disable panic)**:云端把新音量随心跳下发后,
  `stream_task`(**PSRAM 栈**)直接调 `bb_device_config_note_volume_pct` → `persist_config` →
  `nvs_set_blob` 写 flash,flash cache 被关的瞬间 PSRAM 栈不可访问 →
  `esp_task_stack_is_sane_cache_disabled()` 断言 panic 重启(设置菜单调音量不崩,因为走内部 RAM 栈的
  commit_task,ADR-037)。修法:`persist_config()` 的 NVS 写改到**一次性内部 RAM 栈任务**上跑,快照
  用 `MALLOC_CAP_INTERNAL` 分配——所有云端驱动的持久化路径(`note_volume_pct` / `apply_update` /
  `apply_welcome`)一律 cache-safe。
- **固件:云端 URL 去掉 `:38082` 端口,走标准端口/反代(`http://bbclaw.daboluo.cc`)**:运行时云端
  API 原硬编码 `:38082`,但云端已迁到标准端口反代(OTA flash-bundle 端点早已是无端口的
  `bbclaw.daboluo.cc`)——设备仍连 `:38082` → 连不上 → "CLOUD ERR" 联网报错(正是触发上面密语
  锁死的那次网络错误)。三处同步:C 默认(`bb_config.h`)、生产构建 `sdkconfig.bbclaw.latest`、
  Kconfig 默认(`Kconfig.projbuild`)。
- **固件:密语(miyu)锁屏在断网时把人锁死(ADR-039)**:密语解锁只能走云端 `voice.verify`,
  但「是否进/留在锁屏」原本只看 `cloud_saas && miyu_enabled`、**不看网络**——于是网络报错后
  闲置 120s 照样息屏进锁屏,再也校验不过 → 永久锁死(开机即锁/锁屏后才断网同理)。两处协同修:
  ① **断网不自动锁屏**——闲置升级 `CHAT→LOCKED` 加 `s_transport_health_ok` 门槛,离线停在
  CHAT/STANDBY,网回来后 ≤120s 恢复正常锁屏;② **锁屏期间云端持续不可达 15s 自动解锁回 CHAT**
  (`BBCLAW_LOCKED_OFFLINE_AUTO_UNLOCK_MS`)——救「开机即锁但没网」「锁屏后才断网」。owner 拍板:
  离线优先「别被锁死」,密语防护让位(拔网即可绕过密语,可接受)。
- **固件:OTA 在内部 RAM 碎片的设备上「OTA apply task create failed」装不上**:`ota_apply` 任务栈
  12KB **必须**在内部 RAM(下载+刷 flash 冻结 cache,PSRAM 栈会 panic→reflash loop,issue #179),
  但原本是用户确认 OTA 时才懒加载——那会儿 dot-matrix UI/CJK 字体已把内部堆打散,最大连续块
  <12KB → `xTaskCreate` 失败 → 设备拿不到更新(OTA 是所有其他修复的下发通道,装不上=全卡死)。
  改为**开机预热**:`bb_radio_app_start` 最早一步就建好 `ota_apply` 任务并让它 `ulTaskNotifyTake`
  阻塞,确认 OTA 时 `xTaskNotifyGive` 唤醒。12KB 内部栈在堆还干净时一次性占住、本次开机复用,
  确认时永不再 race 碎片。runbook: `firmware/docs/debug/internal-ram-ws-task.md`。

### Changed
- **固件:`make release-local` 一条命令本地快速发版**:封装好——未设 `OTA_ADMIN_KEY` 时自动
  `ssh $(OTA_DEPLOY_HOST)` 从生产 cloud.env 取 key,内层 `make build` 自带 `$(IDF_SOURCE)` 自源
  ESP-IDF,无需先手动 `get_idf`/`export`。**不传 VERSION 时自动定版**:查 OTA 当前 active bundle
  版本 → patch+1 → 追加 `-g<短哈希>[-dirty]`(OTA 的 VersionGreater 只比 M.m.p、忽略后缀,所以
  patch 真 +1 才能升级;后缀作本地构建可追溯标记;不依赖 git tag——它可能落后于已推 OTA 的版本)。
  `make release-local` 即生产构建+推 OTA bundle(全量,非灰度)。约定:tag→CI 发布(push `v*` 触发
  release.yml)只在显式要求时做,默认走本地快速发版。

### Added
- **固件:密语解锁失败时显示识别到的语音文本（ADR-038）**:密语(锁屏语音解锁)ASR 准确度不稳,
  失败时用户原先不知道设备听成了什么、没法调整。现在解锁失败的锁屏会显示「听到「<识别文本>」请
  重说」。**纯固件改动**——云端 `voice.verify.result` 早已回 `transcript`(`voiceprint_api.go`),
  只是固件没解析没显示。落地:`bb_voice_verify_result_t` 加 `transcript` 字段 +
  `parse_voice_verify_result` 解析 + `bb_page_locked_show_heard()` 覆写锁屏 hint +
  `bb_display_show_heard()`(状态变量 `s_locked_heard` 并入 refresh,防中途刷新冲掉) + reject 分支
  调用并把失败停留 1200→2500ms 给用户时间读。成功即解锁不显示;transcript 缺失降级原提示、不回归。
  `idf.py build` 通过。本仓不打 tag,发布由 owner 触发(同 ADR-037)。
- **固件:设置菜单新增密语(miyu)开关（ADR-037）**:cloud_saas 模式下设备端可自己开/关**密语**
  (锁屏语音解锁),不必再依赖云端/CLI/管家。落地照抄 TTS 开关 + 音量持久化范式:新增
  `MAIN_ROW_MIYU` 行(仅 cloud_saas 显示)、`bb_device_config_set_miyu()` setter(clamp 0/1、
  bump version、存 NVS)、`COMMIT_KIND_MIYU`(commit_task 内部 RAM 栈做 NVS 写)、进设置时从
  `s_config.miyu_enabled` 读入、渲染 "Miyu: on/off"、点击在位翻转持久化。**重启生效**
  (密语只在开机时决定锁不锁屏,本地切换天然下次开机生效;也避免开密语时把自己锁在设置页外)。
  `idf.py build` 通过(8% 余量)。注:本地 build 仅编译验证,真正 OTA 发布须用生产配置
  `sdkconfig.bbclaw.latest` + tag 注入版本(CI 触发);灰度走现有云端 OTA 渠道。**本仓不打 tag,
  由 owner 触发发布。** 待真机验证。
- **adapter_v2 管理页支持设备控制（密语 / 音量），走与 CLI 同一条云端路**:管理页「即时生效」组
  新增「设备控制」区,可开/关**密语**(miyu)与设音量,作用于当前连接的设备——和管家用语音调的是
  **同一条路**。落地:新增 loopback-only `GET/POST /v1/admin/device-config`,POST 复用 `device
  set-volume/set-miyu` CLI 的 `postDeviceConfig`(→ 云端 `POST /v1/devices/{id}/config` → WS
  下发固件),目标设备由 `curdevice.Get()` 解析,无需填 id;无设备/未配对云端时返回友好提示
  (NO_DEVICE / CLOUD_ERROR,ok=false 不报错)。前端密语开/关按钮 + 音量输入,即时下发、无需保存。
  `deviceConfigSetter` 可注入,单测覆盖 GET 状态 / POST 密语 / 音量 clamp / 无设备 / 空 body / 405;
  真机起 adapter DOM 验证:区块渲染、按钮 POST、友好提示均正常。(adapter_v2,暂不打 tag)
- **adapter_v2 管理页展示用户画像 / 记忆（ADR-036）**:设置页新增「只读信息」组,把管家在
  `workspace/MEMORY/` 里积累的关于用户的记忆(`profile.md` 用户档案 / `preferences.md` 偏好 /
  `projects.md` 项目 / `decisions.md` 决策,ADR-020/022)**只读展示**出来,让用户看到管家都记住了
  什么。落地:新增 loopback-only `GET /v1/memory` 读 MEMORY/*.md(canonical 维度优先排序、缺目录
  降级空提示、单文件 64KB 上限);前端新增「用户画像/记忆」section(每文件名 + `<pre>` 内容),与
  「状态/只读」同归新的灰色「只读信息」导航组。管家在对话中自行读写这些文件,本页仅查看。真机起
  adapter DOM 验证:4 个记忆文件按序渲染、profile 内容正常展示。(adapter_v2,纯展示,暂不打 tag)
- **adapter_v2 加项目改一步到位（ADR-036）**:owner 反馈「选目录确认了就直接加，不要下面那套
  表单」。去掉 name/用途/CLIBin 输入 + 「添加项目」按钮,改成单个「+ 添加项目目录…」按钮——点→
  宿主机原生选文件夹对话框→确认即 `POST {path}` 落库,name 由目录名自动推断(无原生选择器时降级为
  `prompt` 粘贴路径)。`summary`/`cliBin` 后端字段保留(可经 API 设,行内编辑留作后续)。真机起 adapter
  验证:单按钮、旧表单输入全消失、add-by-path 自动取名均正常。(adapter_v2,纯前端,暂不打 tag)
- **adapter_v2 管理页重设计为 VSCode-settings 风格（ADR-036）**:左侧 section 导航(scrollspy 高亮)
  + 右侧分区内容,按两套保存模型分两个色标组——「即时生效·无需保存」(绿,项目区,各控件自己的
  按钮即时写入)与「系统配置·需保存」(青,ADR-025 设置)。**取消顶部全局「保存」按钮**,改为
  **底部贴附的上下文 save bar**:仅当系统配置有未保存改动时浮现「有未保存的更改 + 保存」,保存后变
  「已保存·重启后生效 + 重启并应用」;项目区输入无 `data-path`、永不触发 save bar,两套模型彻底
  解耦。每项设置 label+说明+输入(VSCode 式)。真机起 adapter 截图验证布局/save bar/CLI 徽标/
  scrollspy 均正常。(adapter_v2,纯前端,暂不随发布打 tag)
- **adapter_v2 项目载入 P1（ADR-036）**:管家现在开口即知用户在管理页登记的项目,不再出现
  「问了不知道」。落地:新增 `internal/projectstore`(sibling `~/.bbclaw-adapter-v2/projects.json`,
  移植自 v1、原子写、加 `summary`/`cliBin` 字段、不限 git 任意目录)、loopback-only
  `/v1/projects` 增删查 + `/v1/admin/pick-dir` **宿主机原生目录对话框**(macOS osascript /
  Linux zenity;管理页 loopback 同机,Finder 选文件夹直接返回绝对路径,不在浏览器渲染目录树)、
  管理页项目卡片。**持久触达管家**:`DeviceSystemPrompt` 在 boot 时读 projects.json 把项目渲染成
  紧凑清单(名字—用途—目录—[CLI 就绪/未配置])注入 `--append-system-prompt`,进系统提示=每轮强
  加载、保证不遗忘。**加/删项目不强制重启**(owner 反馈:配置项目是低优先级 setup、不用立刻生效)——
  只持久化,下次适配器自然重启时焊进系统提示。项目名并入 `ASR_HOTWORDS` 提升语音识别;`cliBin` +
  `cliReady`(LookPath/Stat)预防 `BBCLAW_*_BIN` 空导致调不到工具。`go test -race ./...` 全绿。
  **待补**:移植 v1 prewarm 的 scanner 写 `MEMORY/projects.md` 重知识(§决策三);dispatch 按 ADR
  拆为后续。(adapter_v2,骨架阶段,暂不随发布打 tag)
- **adapter_v2 计划清单采集探针（ADR-034 P0 第一步，默认开、纯观测）**:为后续把
  claude `TodoWrite` 计划清单渲染到固件屏幕（display-only `task.list` 通道）做准备,先落
  **数据采集探针**——`extract.ScanTaskListCandidates` 用放宽的字形集（ballot box / 方块 /
  圆 / 对勾叉 + ASCII `[ ]`）扫可见网格找 checkbox 样式行,`deviceapi.captureTaskListProbe`
  在每次 PTY 重绘后跑,命中连续 ≥2 行同缩进块时打一行**紧凑去重日志**,记录每行前导**字形
  U+码点**（`☐`/`□`/`◻` 码点不同但肉眼难辨,故记码点不记肉眼字形）+ 文本。**默认开**（真机
  命中 TodoWrite 渲染是概率性的、没法按需复现,藏在 env 后会漏抓）;`ADAPTER_V2_DEBUG_TASKLIST`
  额外 dump 整屏 tail 供复原 fixture。纯日志观测,不改抽取/朗读行为。待真机留下样本后据此钉死
  字形→status 映射,再写严格识别器 + `task.list` 帧。`go test -race ./...` 全绿。
  (adapter_v2,骨架阶段,暂不随发布打 tag)
- **adapter_v2 管家「自身音量控制」(参考 v1 移植)**:管家(butler)现在知道如何调节
  **用户正在使用的这台设备**的音量与密语模式。落地:新增 `bbclaw-adapter-v2 device
  set-volume <0-100>` / `set-miyu <on|off>` CLI 子命令(走云端 config API,等价 v1 的
  `bbclaw-adapter device`,云端再经 WS `config.update` 推给固件生效);persona
  (`DeviceSystemPrompt`)恒注入「设备控制」技能段教管家用 Bash 调它。与 v1 不同:v2 的
  persona 是开机一次性构建、跨设备共享(P1),无法像 v1 那样按轮注入 deviceId——改由新增
  `curdevice` 在设备(devicews hello / cloud relay 请求)连上时记录「当前设备 id」到数据目录,
  CLI 默认即作用于当前设备,管家无需知道 id。`$BBCLAW_ADAPTER_V2_BIN`(main 导出运行二进制
  绝对路径)让管家无视 PATH 也能调到。仅 cloud_saas(云端配对)可远程改;失败时 persona 要求
  如实告知。`go test ./...` 全绿。(adapter_v2,骨架阶段,暂不随发布打 tag)
- **adapter_v2 阻塞弹窗 → 设备确认（ADR-033，P0 adapter-only，opt-in）**:claude 交互
  TUI 的权限/工具确认菜单（`Do you want to proceed? ❯1.Yes 2.… 3.No`）现可被识别、转发
  设备确认、把所选数字注入回 PTY（数字直提，真机验证）。落地:`extract.ParsePrompt`
  label 锚定识别器（Yes…/No… 形状 + ❯/页脚佐证，prose 列表不误判）、boundary §0 勘误
  修复（弹窗不再被误判成 turn 结束→播空白）、`deviceapi` PromptObserver/SelectPromptOption
  + 单一 promptPending 门 + 超时/无设备 auto-DENY（绝不 auto-approve）+ supersede/respawn
  作废；设置 `ADAPTER_V2_CONFIRM_ON_DEVICE`（默认 off=旧 bypass 行为不变）开 → butler 走
  `--permission-mode default`。经 4 维度多智能体对抗审查 + 5 处修复，`go test -race ./...`
  全绿。设备端菜单渲染（devicews）+ cloud parked-turn + firmware 为 P1/P2。
  (adapter-only,需 tag 才随发布出二进制)

### Fixed
- **固件:云端远程调音量后,设备「设置」菜单仍显示旧音量(如 65%)**。管家/远端经云端
  `device set-volume` 改音量时,固件经心跳轮询拿到 `cloud_volume_pct`,运行时变化时
  `bb_radio_app.c` 只调 `bb_audio_set_volume_pct()` **应用了音频却没回写本地 `s_config`**
  (`bb_device_config`)。而「设置」菜单进入时读的正是 `s_config.volume_pct`
  (`bb_ui_settings.c` 入口),于是音量实际变了、菜单却一直显示改之前的百分比(开机重放
  也读 `s_config`,故重启后又被旧值盖回)。修复分两处,覆盖两种时序:
  - **关菜单时改 → 再进菜单刷新**:云端音量运行时变化、应用音频的同时,新增
    `bb_device_config_note_volume_pct()` 把值回写本地 config 并持久化到 NVS——**故意不 bump
    version**(该值源自云端,抬高本地 version 会让后续云端 versioned `config.update`/welcome
    被 `version<=current` 拦掉)。`bb_ui_settings_show()` 进入时重读 `s_config` 即拿到新值。
  - **菜单正开着时改 → 实时刷新**:新增 `bb_ui_settings_notify_volume_pct()`,心跳应用云端
    音量后(包 `lvgl_port_lock`)调用它,直接更新当前显示的音量行/进度条;菜单关闭或用户正在
    手动调音量(`volume_dirty`)时为 no-op,不打架。
  `make build` 通过。(注:`speaker_enabled` 走同一云端心跳路径,但它压根没持久化进 `s_config`、
  开机不重放、「设置」也不显示,是另一类独立 gap,本次未动。)

### Docs
- **ADR-036：adapter_v2 项目载入与系统提示项目清单注入**（`design/decisions/ADR-036-...`）。
  复盘截图失败（管家被问 Buildhub 答不上来）两根因=系统提示无项目清单 + `BBCLAW_BUILDHUB_BIN`
  空/工具调不到,提出把 v1 的项目载入能力带回 adapter_v2,但**接到已有基础设施上而非另起炉灶**:
  移植 v1 `projectstore`（sibling `projects.json`）+ loopback-only 管理页 `/v1/projects` 增删查
  + 内置目录选择器(非程序员可点选任意目录、不限 git);**关键**=项目清单在 boot 时渲染进
  `DeviceSystemPrompt`、加/删项目复用现成 `adminapi.Restart()` re-exec 落地（进系统提示=每轮强
  加载,这是「保证不遗忘」的正解,非靠管家自觉读 `MEMORY/projects.md`;明确否决 respawn——只换
  会话 id 不重建 baseArgv);重知识由移植自 v1 prewarm 的 scanner 写 `MEMORY/projects.md`;热词
  无独立引擎(项目名进系统提示做 LLM 意图映射 + 并入 `ASR_HOTWORDS` 做 ASR boost);新增 `cliBin`
  字段 + `cliReady` 状态正面预防「BIN 空」。否决 driver-plugin 方案(v2 P1 无 driver 层)。4 项关键
  决策已与 owner 确认:re-exec 生效 / P1 只做「知道+能调 CLI」不含 dispatch / 单一共享项目池 /
  内置目录选择器。状态:草案,代码未实现。(docs-only,不打 tag)
- **ADR-034：adapter_v2 计划清单（TodoWrite）→ 设备 display-only `task.list` 通道**
  （`design/decisions/ADR-034-...`）。把 claude TUI 的 `TodoWrite` 计划清单（每条带
  pending/in_progress/completed 勾选态）抓屏抽取成一条新的 display-only 下行通道
  `task.list`，整张快照推到固件渲染成进度面板（用户看得到「多步任务做到第几步」），
  延续 ADR-030 双通道纪律（永不进说通道、TTS 零变化）+ ADR-033 抓屏方法论（fixture 硬
  发布门、parse 失败 fail-safe 不显示）。无回路 / 无能力协商 / ignore-unknown，是四端里
  最易增量上线的通道。澄清与 ADR-021-firmware-ui「Task List」页的关系：同一固件清单页
  概念，v1 用 butler-dispatch 结构化事件源，v2（单 PTY 无 worker dispatch）用本 ADR 的
  抓屏 TodoWrite 源。字形→status 映射为 P0 真机探针闸门（未跑探针不臆测硬编码）。
  状态：草案，代码未实现。(docs-only，不打 tag)
- **ADR-033 / 设计：adapter_v2 阻塞式交互弹窗 → 设备确认**（`design/adapter_v2_blocking_prompt_confirm.md`
  + `design/decisions/ADR-033-...`）。把 DESIGN.md §9（tool 审批 scrape）泛化为「所有阻塞式
  claude TUI 弹窗」：截屏识别权限 / 工具确认弹窗 → 转发 `{问题, 选项}` 到设备 → 选择注入回 PTY；
  装饰类弹窗（upsell / 打分 / trust / onboarding）走配置压制。含一处正确性勘误（boundary 把弹窗
  误判成 turn 结束 → 播空白 / 脏音频）、单一 `promptPending` 门协调既有注入器、cloud parked-turn、
  安全不变量（破坏性动作超时 / 无设备 auto-DENY、只按键确认）。状态：提议，两个 P0 探针**已真机验证**
  （注入=数字直提，无需箭头 / HighlightedRow；权限菜单需显式 `--permission-mode default` 才弹，默认
  部署的 auto-accept 会吞掉），代码未实现。(docs-only，不打 tag)

### Added
- **opencode serve+SDK 后端（ADR-031，opt-in `AGENT_OPENCODE_SERVE=1`）**:opencode driver
  新增一条经 `opencode serve`(OpenAPI/官方 Go SDK)驱动的后端,取代「每轮 spawn
  `opencode run` + scrape NDJSON」。落地能力:`serveManager` 懒启动 + 监管常驻 serve +
  `/global/health` 版本握手(支持区间 `[1.15, 1.30)`,越界拒绝并提示安装) + 崩溃重启;
  流式 `EvText`/`EvThinking` delta、`Interrupt`(abort barge-in)、原生 `system` 注入、
  per-turn model;可选能力 `ModelLister`/`SessionLister`/`MessageLoader`/`PartLoader`/
  `CLISessionChecker` 全实现。事件流弃用 SDK typed-union、改裸 SSE 读 `/global/event` +
  按 type/raw-JSON 解(对 SDK 版本滞后免疫)。旧 CLI driver 默认不变,迁移可逆。
  (adapter-only,需 tag 才随发布出二进制)
- **opencode serve 后端的 butler MCP 派发(ADR-031)**:`StartOpts.MCPServers` 经
  `POST /mcp` 注册到共享 serve(每 server 名幂等一次);butler 调 `bbclaw_dispatch`
  工具时,router 把 tool 状态流映射成 `EvDispatchStatus`(started→async/done→error,
  带 cwd/title/elapsedMs/childSessionId),对齐 claudecode 的 `mcp__bbclaw__dispatch`
  语义;`PartLoader` 历史里的 dispatch 也归 `dispatch` kind。新增 `serve_dispatch.go`
  + dispatch 识别/解析单测 + MCP 注册 live 测试。设备审批 UX 仍为 fast-follow。

- **opencode serve 开关上管理页（ADR-031 / ADR-025）**:把 `AGENT_OPENCODE_SERVE` env 开关
  收敛为持久化设置 `ai.opencode_serve`(settings.json)——admin「AI 配置」页一个勾选框,保存后
  重启生效。`config.OpencodeServeEnabled()` web override 优先、回落 env(env 转为首次 bootstrap
  种子)。沿用 `CloudRelayOverride` 同款「web 设置覆盖 env」链路(settingsstore FromConfig/ApplyTo)。

### Changed
- **PTT 打断改为「撤回」语义：撤掉被打断的回合，不注入打断备注（ADR-028 §2.5.1 修订）**：
  对齐 Claude TUI「Esc 取消 → 已发内容撤回到输入栏、当没发生过」的心智模型。
  手势（前提：后台还在处理中）——**单击 PTT 无音频 = 撤销**（不发送，并把刚发出、
  正在处理的那条提问 + 它的半截回复从屏幕和缓存撤掉，保留更早的完成对话）;
  **按住重说 = 撤销之前的 + 把现在这句重新处理**（旧轮先撤，新轮正常 append）。
  - **adapter**：`turn.cancel` 原先会在下一回合 `claude -p --resume` 的 prompt 前注入
    `[系统提示：你上一条回复在「…」处被打断…]` 备注;现改为 **只杀回合子进程
    （保留 session/resumeID），不记录、不注入任何备注**。`InflightRegistry` 删
    `notes`/`NoteInterruption`/`ConsumePromptNote`、`Cancel(deviceID)` 去掉 `playedText`
    入参;`engine.go` 删 prompt 注入块;`POST /v1/agent/cancel` 与 homeadapter
    `turn.cancel` 同步;e2e 测试改判第二轮为干净用户文本。
  - **firmware**：新增 `bb_chat_transcript_withdraw_last_turn()`（按 `s_last_user_bubble`
    锚点删被打断那一轮的全部 bubble）+ `bb_chat_cache_drop_last_turn()`（截断缓存到最后
    一条 user 之前，避免休眠/重进会话又冒出来）;barge-in 取消的两个安全点触发——
    语音→agent 路 `ABORTED_BY_USER`、本地 agent 路 `agent_task` 丢弃结果时,经
    `safe_lv_async_call` 排到 LVGL 任务 FIFO 末尾执行（迟到的 chunk 一并清掉）。
    「没音频不发送」沿用既有 VAD 阈值门（松手 `skip_finish`）。
  - **底层限制**：Claude Code 增量持久化 JSONL 仍留半截 assistant 输出，`--resume` 时
    模型能看到截断的自己（彻底「当没发生过」需 JSONL 手术，未做）。
  - 含 firmware + adapter 改动,**需 tag 才随发布出固件 OTA + 适配器二进制**;
    barge-in 三场景按惯例须真机验证后再打 tag。

### Fixed
- **没 WiFi 时无限重启：配网 softAP 启动崩溃（#252，真机验证）**：连不上任何已知 wifi
  → 进配网 AP → `esp_wifi_start()` 在闭源 wifi 库 `ieee80211_hostap_attach` 对 NULL
  做 `strlen` 崩溃（`LoadProhibited`）→ `rst` 重启 → 死循环。根因:**dev/bench build
  的 `sdkconfig` 生成时回落 IDF 默认的 static TX 缓冲（16×1.6KB≈25.6KB），`esp_wifi_init`
  把内部 DMA RAM 一把吃光**（实测 largest 29KB→0），softAP 连 752B beacon 都分不出。
  （release `sdkconfig.bbclaw.latest` 早设了 dynamic TX,故只 bench 板复现。）修两处:
  ① `bb_wifi.c` **运行时强制 dynamic TX**（`cfg.tx_buf_type=1`，走 PSRAM via
  `SPIRAM_TRY_ALLOCATE_WIFI_LWIP`），不依赖 sdkconfig 生成——wifi init 内部 DMA 占用
  29KB→26.6KB（仅 ~3KB），softAP 正常起；② softAP 启动前加**内部 DMA 守卫**
  （低于 `BBCLAW_WIFI_AP_MIN_DMA_*` 门槛则优雅返 `ESP_ERR_NO_MEM`，上层停 WiFi 错误页
  而非让 wifi 库崩）作纵深防御。真机:配网 AP `BBClaw-Setup-xxxxxx`@192.168.4.1 正常起、
  配网页显示、单次启动、零崩溃。
- **按键自测任务偷内部 RAM 致 PTT 录音流的 WebSocket 建不起来、语音识别失效**：
  `bb_button_test` 任务用普通 `xTaskCreate` 申请 3072B **内部** RAM 栈,而 bbclaw 板
  `board_config.h` 把 `BBCLAW_BUTTON_TEST_GPIO` 设为 1 → 正式板也常驻该任务。内部堆本就
  贴着 8KB 线(点阵 UI 数百小对象 + 近期 CJK 字体/Sessions 菜单加压),这 3KB 把最大连续块
  挤到 7680B < adapter ws 任务所需 8192B → `Error create websocket task` / `ws start failed`
  / `bb_adapter_stream_start failed ESP_FAIL` → 录音帧采到却送不出、服务端无音频、ASR 无输入、
  agent 直接 DIZZY。把该任务栈改用 PSRAM(`xTaskCreateWithCaps`+`MALLOC_CAP_SPIRAM`),内部 RAM
  零占用——与 `bb_uart_cmd` 的同类修复(7899e19)一致。

### Design
- **ADR-031 — OpenCode 作为 canonical 后端**（草案，方向已定 + 1 个 spike 闸门）:
  把 adapter 的「每家 CLI 一个 scrape driver」动物园收敛为单一 canonical 后端
  OpenCode——由它在内部适配 75+ provider，adapter 只对接它一个**带版本的稳定 API**
  (`opencode serve` OpenAPI 3.1 + 官方 Go SDK)，取代现在「每轮 spawn `opencode run`
  + scrape NDJSON」的弱 driver。沿用 `claude` 同款 BYO 安装模式（用户自带 opencode、
  发版约定依赖版本、运行时 `GET /global/health` 握手强制版本区间），**不打包**那个
  ~106MB 二进制。实测映射证明是净升级（白拿 tool approval / interrupt / 历史回放 /
  原生 SubtaskPart 派活）。重评 ADR-024 的多-driver 前提，待合并评审。
  （docs-only，无固件/adapter 二进制改动，不触发 tag）

## [0.5.15] - 2026-06-17

### Fixed
- **流式中文回复乱码(UTF-8 被切断)**:对话回复的 coalesce 缓冲(`post_assistant_chunk`,
  `bb_ui_agent_chat.c`)在缓冲将满时按**字节**截断 delta,把一个 3 字节中文字切成半个并丢弃
  尾字节 → 渲染成豆腐块/乱码。改为截断时调已有的 `utf8_safe_truncate()` 落在字符边界
  (TTS 路径早就这么做、渲染路径漏了);丢掉的尾部由回合结束的权威历史 fetch 补回。
- **系统繁忙时按键积压、恢复后逐个重放(补操作)**:nav 事件用 per-type 版本计数器,
  stream_task 繁忙时(cloud_wait / TTS i2s_write 阻塞主循环数秒)无法消费,版本号累积,
  恢复后 `while(handled!=cur)` 把繁忙期间狂按的每一次都重放一遍 → UI 乱跳。改为消费时
  **折叠**:一串同键 backlog 最多触发一次;并给每个 nav 事件记按下时间戳,若最新一次也已
  超过 `BB_NAV_EVENT_STALE_MS`(1500ms,远高于循环最坏 ~250ms idle delay,正常按键绝不误丢)
  则整批丢弃——几秒前按的键不会在系统空闲瞬间突然生效。(`bb_radio_app.c`)

### Removed
- **删除死的 LVGL 元素图资覆盖文件 `bb_lvgl_img_elements.c/.h`(327+38 行)**:全树零引用
  的手写资源覆盖,含 12 个早已废弃的角色 idle 动画帧(red/blue/green ×4)+ avatar/mic/claw/
  battery plate。删除后 CMake 自动回退到生成基线 `generated/bb_lvgl_element_assets.c`(由
  `make gen-lvgl-elements` 维护)。链接器 `--gc-sections` 本就 GC 掉了这些未引用数据,故不省
  flash,纯属源码清理(去掉死代码 + display.c 的重复 include)。注:调查确认其余动画(boot
  扫描 / 锁屏钥匙孔呼吸 / 待机冒号呼吸 / 对话平滑滚动 / 底栏 motif / 录音 VU)**均在用**,未动。

## [0.5.14] - 2026-06-17

### Added
- **设备端执行步骤展示(ADR-030)**:管家执行一次工具调用(如
  `Bash: bbclaw-adapter device set-volume 20`)时,对话页现在会显示一行简要的
  执行步骤(`[tool] Bash: …`),而**不会**被语音播报。明确区分两条通道:
  - **主内容通道**(`reply.delta` / `voice.reply`)= 展示 **且** TTS 播报;
  - **步骤通道**(`tool_call` / `thinking` / `dispatch_status`)= 只展示,**永不**进入
    TTS。

  后台因此只播报「音量已调到 20%」这类主结果,避免把大量进行中文本读出来。
- **工具步骤携带 hint**:`tool_call` 帧新增 `hint` 字段(命令 / 文件路径预览,
  ≤80 字),对话页步骤行显示有意义的内容而不只是裸工具名。向后兼容:旧固件忽略
  `hint`,旧适配器不发 `hint` 时按原样渲染。跨端联动:adapter
  `homeadapter/adapter.go` 发送 `{name, hint}`,cloud relay 透传 `hint`
  (`server.go` 两处 + `chat_api.go`),firmware 解析并在对话页
  (`on_finish_stream_event_tts_only`)渲染为步骤行。

### Fixed
- **切换 Adapter(机器)后会话/历史不跟着切**:此前切机器后重拉新机器的 driver,
  两台机器 active driver 同名(都 `claude-code`)→ `bb_ui_agent_chat_set_active_driver`
  撞上「同名 no-op」短路,session 和历史**根本没重载**,画面停在旧机器的会话,且
  `session_id` 仍是旧机器的(下一条消息会带旧 session 发到新机器 → `SESSION_NOT_FOUND`)。
  改走新路径 `bb_ui_agent_chat_resync_after_adapter_switch`:**不按 driver 名 no-op**,
  丢掉旧机器的 session+transcript,向新机器 `GET /v1/agent/sessions` 要会话列表、取最近
  一条、实时拉它的历史——**不在设备上按机器存多份,始终从后台拉对应机器的数据**。聊天浮层
  关着时(从待机进设置切机器)走延迟标志,下次开聊天时从后台解析,避免读到旧机器的 NVS 会话。
  会话查询任务跑 PSRAM 栈(HTTPS,无 NVS)。实测:切到另一台机器后自动加载了它那条 374 条
  消息的会话历史。

## [0.5.13] - 2026-06-16

### Changed
- **设置页去掉「Back」菜单行**:编码器长按本来就是 BACK(主页长按直接退回
  chat,子选择器长按退一级),那行 Back 是功能冗余。改为主页底部一行 dim 提示
  `hold to go back`,菜单只剩真正的设置项(Driver / Model / Adapter / Vol / TTS)。
- **设置页菜单循环导航**:↑/↓ 到顶/到底后回绕到另一端(原来是夹住不动),
  主页和所有子选择器(Driver/Model/Adapter/Vol)一致。
- **Adapter 在线/离线状态显式展示**:Adapter 选择器每行都标 `[on]`/`[offline]`
  (原来只在离线时挂个 `(offline)`,在线无标记);主页 Adapter 行也带上绑定
  adapter 的实时状态(如 `macbook [on]`)。离线 adapter 仍然列出、仍可选中。
- **状态灯改红绿灯配色(红/绿/橙)**:原来 P3 思考 / P4 倾听 / 新通知脉冲 /
  boot marquee 用蓝色,现统一换成橙 `(255,165,0)`。RYG 三线板上经 Y=R+G 近似
  为黄灯(交通灯黄/琥珀),WS2812/RGB 模块上为真橙;不再用蓝。
- **退出设置回到对话页而非待机页**:从设置页(OK-Back / 长按 BACK)退出后,落到
  聊天对话浮层而不是待机时钟视图——退菜单应回到对话上下文。密语锁定态下仍走锁屏不受影响。
- **密语(miyu)运行时关闭即时解锁(修 bug)**:此前在 LOCKED 锁屏态被云端
  `config.update` 远程关掉密语后,设备无任何解锁路径会卡死在锁屏。现在主循环检测到
  密语已关 + 处于 LOCKED 就立即解锁回 CHAT,无需重启。

- **修复设置页 Adapter/Driver 全空 + 切换静默失败(内部 DRAM 碎片)**:设置页打开时
  内部 DRAM 最大连续块会跌到 ~7.7KB(< 8KB 任务栈),导致 `xTaskCreate` 失败——
  driver 列表 / sites.list 的 fetch 任务**根本没创建**,设备从不向云端查询,于是
  Driver 显示 `offline`、Adapter 显示 `(none)`,**与云端是否有在线 adapter 无关**
  (实测云端 2 台在线时设备仍全空)。同一碎片还让 adapter 切换 / 音量 / TTS / 模型
  提交任务建不起来,日志报 committed 实则没执行。修法(`bb_ui_settings.c`):
  - **纯网络任务挪 PSRAM**(`xTaskCreateWithCaps`):driver 列表 fetch、sites.list
    fetch、MODEL 提交(HTTPS PUT)、ADAPTER 提交(WS sites.activate)——均无 NVS/flash
    写,PSRAM 栈对 cache-freeze 安全。
  - **DRIVER 提交拆分**:HTTPS PUT 走 PSRAM,其 NVS 写(记录 active driver)拆到独立
    4KB 内部小任务,绕开「PSRAM 栈 + NVS cache freeze 会 fault」的约束。
  - **VOLUME/TTS 持久化缩栈**:仅 NVS set+commit,8KB→4KB 内部栈,可靠塞进碎片堆。
  - WithCaps 任务配 `vTaskDeleteWithCaps` 自删,避免 PSRAM 栈/TCB 泄漏。

### Notes
- 设置页打开时 PTT 已被丢弃(`s_settings_active` 早返),本次确认无需改动。

## [0.5.12] - 2026-06-16

### Changed
- **对话 UI / 历史滚动重做(往最精简 + 静态齐屏)**:
  - **底栏动画精简**:只在语音**输入(LISTEN)**时动(给采集反馈),**输出(SPEAK)/思考
    (PROCESS)/空闲(IDLE)一律静态一帧** → 屏幕安静,边听 TTS 边翻历史更顺。修复了此前
    follow-tail 守卫误把"听写动画"在用户翻过历史后冻掉的回退。
  - **输出阅读不重排**:TTS 流式回复时若用户上翻读历史(reading mode),新到的文字只进
    缓存+暂存、不再每块 `lv_label_ins_text` 重排整列(那是边听边翻的卡顿源);翻回底部 /
    新一轮 / 回复结束时一次性补齐,不丢字、不乱序。
  - **滚动加速**:按住 ↑/↓ 连续重复时步长递增(2→…→16 行/步,封顶),长按快速到顶。
  - **上/下翻方向翻转**;**录音输入时禁用上下翻**(说话时无需翻、也让采集路径不被占)。
  - 上翻分页批量 24→8(每次到顶加载的突发更小、更顺);渲染上限收到 120 条。
- **adapter CLI 新增 `device set-miyu <on|off>` 子命令**(#190):对标 `set-volume`,
  通过 `POST /v1/devices/{id}/config` 远程开关设备「密语模式」(即 `miyuEnabled`——
  cloud_saas 锁屏语音解锁开关),Cloud 经 `config.update` 实时下发到设备,无需重启。
  AI butler 的设备控制提示(`DeviceSystemPrompt`)同步加入 `set-miyu` 用法,可口头响应
  「开/关密语模式」。`setConfigRequest` 改为指针字段 + `omitempty`,确保 `set-miyu` 不会
  误发 `volumePct:0` 把设备静音。

### Fixed
- **时间分隔符乱码**:`── HH:MM ──` 用的制表符 U+2500 不在中文字库子集 → 渲染成乱码。
  改用 ASCII 短横,并加上日期:`-- MM-DD HH:MM --`。
- **TTS 播完后状态灯/底栏延迟 ~2s 才转绿(idle)**:回 IDLE 只靠从 cloud_done(文本完成)
  起算的固定 3s DIZZY 定时器,短回复音频早于 3s 放完仍空等残余 → 音停后还蓝着一两秒。
  改为**音频播放结束即触发 DIZZY→IDLE**,底栏+灯立刻转绿(长回复/barge-in 不受影响)。

## [0.5.11] - 2026-06-14

### Fixed
- **切换驱动/模型时概率性重启**:settings 的 fetch/commit 任务栈只有 4096 字节,而一次
  model commit 要跑 HTTPS PUT(TLS 握手)——v0.5.9 改用软件 AES 后密码运算也压到栈上。
  真机实测 model commit 峰值用栈 **3980 字节**,在 4096 栈下仅剩 **116 字节**余量,稍有
  波动(证书链/握手分支)就栈溢出 → 设备重启(故表现为「切模型时概率触发」)。栈提到
  **8192**(ESP-IDF HTTPS 任务常规值),实测余量回到 4212 字节;压测连续切 driver+model
  无重启。`commit_task` 增加一行栈高水位日志便于今后现场核对。

## [0.5.10] - 2026-06-14

### Changed
- **设置入口改为单击 OK(取代不可靠的长按)**:原先进设置要长按 OK 700ms 触发 BACK
  手势,但没有触觉反馈、用户普遍在到阈值前就松手,长按几乎从不成立(真机实测连续按压
  全部停在 ~150-200ms → 只判成短按)。现在**单击 OK 直接进设置**——待机/buddy 屏和聊天
  视图里都生效(`bb_radio_app.c`:CHAT 态 OK 分支 + 待机唤醒分支各加 OK→SETTINGS)。
  原短按 OK 的 Task List 入口按用户要求移除;长按(BACK)仍保留"忙时取消当前对话"。
  进聊天对话改用方向键 / PTT。

### Fixed
- **历史对话上/下翻滚动卡顿**:空闲时底栏点阵 motif 仍以 48ms 周期重绘(`BAR_IDLE` 的
  breathe 动画),持续占用 LVGL 线程,导致滚动事件被 defer——真机实测每次 UP→实际滚动
  延迟在 **70~335ms 抖动**,并伴随 `lvgl busy — queued` / `lock timeout dropping`。改为
  **空闲时底栏只渲染一帧静态、不再重绘**(`bottombar_timer_cb` 对 `BAR_IDLE` 提前返回),
  LISTEN/PROCESS/SPEAK/ERROR 对话态照常动画。实测滚动延迟降到 **0~16ms 且无抖动/丢弃**,
  翻历史跟手。

## [0.5.9] - 2026-06-14

### Fixed
- **OTA 固件下载在 ~32% 处必崩 `esp-aes: Failed to allocate memory` → OTA 失败**:HTTPS 下载走
  TLS,密码套件用 AES-GCM。配置是硬件 AES(`MBEDTLS_HARDWARE_AES=y`)+ TLS 缓冲在 PSRAM
  (`EXTERNAL_MEM_ALLOC=y`)+ 16KB 接收记录(`SSL_IN_CONTENT_LEN=16384`):硬件 AES 对 PSRAM 里
  的大记录解密时要临时申请一块**连续的内部 DMA bounce 缓冲**,而 OTA 时内部最大连续块只有
  ~10.7KB < 16KB → 分配失败 → `esp_tls_conn_read` 报错 → `bb_ota: HTTP read error` → 整个 OTA
  中止。改 **`CONFIG_MBEDTLS_HARDWARE_AES=n`(软件 AES)**:彻底去掉 esp-aes 的 DMA 分配路径,
  不再吃内部 DMA RAM。本设备 TLS 吞吐很低(opus 音频 + 控制 + 偶尔 OTA),软件 AES 的 CPU 代价
  可忽略;`HARDWARE_MPI`(RSA/ECC 握手加速)保留。**注意:旧固件无法靠 OTA 自愈此问题(下载用的
  就是有问题的 TLS 栈),需 USB 烧录一次带本修复的固件,之后 OTA 才可靠。**
- **idle 超时未判断 OTA 进行中**:OTA 下载期间若静默超过 `BBCLAW_CHAT_IDLE_TIMEOUT_MS`,主循环
  仍会触发 `agent_chat_exit()`/状态切换,干扰 OTA 进度页渲染(不影响 flash 写入,但画面会乱)。
  idle 超时外层条件加 `!bb_page_ota_active()` 守卫。

## [0.5.8] - 2026-06-14

### Fixed
- **长对话中 WebSocket 被云端掐断,整轮回复丢失(`finish_failed WS_DISCONNECTED`)**:云端对设备
  方向的 WS 用 35s read deadline,且**只在收到设备上行消息时重置、服务端自己不 ping**。长 agent
  回合里设备只收 TTS、不上行,静默一过 35s 云端就主动 FIN → 设备侧 `ESP_ERR_ESP_TLS_TCP_CLOSED_FIN
  errno=128`,turn 丢失。真机复现后云端日志实锤 `ws closed role=device ... err=...: i/o timeout`
  每 30-90s 一次。**云端修复(bbclaw-reference)**:服务端每 12s 主动 ping 每个设备/relay 连接,
  且**任何成功的下行写入(TTS/事件/ping)都重置 read deadline**——不再依赖设备回 pong(内存吃紧
  的设备常发不出)。**固件侧(本仓)**:WS 客户端补设 `ping_interval_sec=15` +
  `disable_pingpong_discon=true` 作纵深防御(漏 pong 不触发本地自断,死连接仍由 read-error 重连兜底)。
  实测:云端重部署后两台设备 2.5+ 分钟 0 次超时/断开(此前每 30-90s 必断)。
- **内部 DRAM 在对话中枯竭,致 barge-in 取消发不出 + 偶发流中断**:`CONFIG_SPIRAM_MALLOC_ALWAYSINTERNAL
  =16384` 把所有 <16KB 的裸 `malloc()` 强制塞进内部 RAM,而 TTS 路径每个 chunk 都 `malloc` 一块
  ~10KB PCM,一轮上百 chunk 反复 alloc/free → 内部堆碎成最大连续块 256B、free ~3KB →
  `xTaskCreate`(TCB 必须内部)失败:`turn_cancel: task spawn failed (no mem) — cancel NOT sent`、
  `capture_task ringbuf full`。把 TTS 的 chunk 结构体 + PCM 缓冲(非 DMA、播放时才 memcpy 进 I2S
  DMA)改为 PSRAM 优先分配(`tts_alloc`/`tts_calloc`),opus 编码器 25KB 仍留内部(SILK 需内部)。
  真机实测 `heap after cloud_wait` 内部 free 从 3183→5.9K~13.8K、largest 从 256→1.5K~3.5K,
  `no mem`/`ringbuf full`/`turn_cancel fail` 全部消失。

### Added
- **UART 调试命令 `heap`**(仅 `CONFIG_BBCLAW_DEVICE_MONITOR` dev 构建):按需打印
  internal/spiram free+largest 及 `heap_caps_print_heap_info` 全区明细,现场排查内存碎片用。

### Changed
- **WiFi 自动连接改 scan-then-connect,跳过不在场网络的盲等超时(#182)**:原先是「串行盲连」
  ——已保存 SSID 按最近成功时间戳倒序逐个 `set_config`→等满一个
  `BBCLAW_WIFI_STA_CONNECT_TIMEOUT_MS` 才轮到下一个,排在前面的网络若此刻不在范围内
  (出门/换地点),设备必须白等一整个超时,多网时启动/重连明显变慢。现在连接前先做一次
  全信道阻塞扫描(~1-2s,一次即可并发看到周围所有 AP),给每个候选标记「是否在场」:
  **Pass 0 先连在场命中的(快路径,不盲等),Pass 1 再连其余**(覆盖扫描失败/隐藏 SSID/
  弱信号漏扫,等价原盲连)。扫描失败或交集为空自动回退全量盲连,行为不退化;单 SSID 不变。
  扫描核心抽出 `wifi_scan_collect()` 与 HTTP 配网页共用;预筛期间用 `s_suppress_autoconnect`
  抑制 STA_START 的自动连接与掉线重试,避免空配置连接风暴。ESP32 单射频无法并发*连接*
  但可并发*扫描*,故只在「能连的集合」里按优先级依次连。真机实测:已存 2 个 SSID、仅 1 个
  在场 → 直接连在场那个(`init→auth→assoc→run`),不再盲等缺席网络的超时。

## [0.5.7] - 2026-06-13

### Fixed
- **(ADR-027)Settings「Adapter(机器)」选择器可能永久卡在 `(loading)`**：`site_fetch_task` 在
  `bb_adapter_sites_list` 返回后,若 `lvgl_port_lock(200)` 拿不到锁,旧逻辑直接 `free(r)` 却
  **没清 `site_fetch_pending`** → picker 永远显示 `(loading)`,且因 `spawn_site_fetch_task` 在
  pending 时 early-return,后续再开也不会重拉。改为重试取锁 3 次、最终兜底直接清 pending 并丢结果,
  保证 loading 态不会永久卡死(正常路径 cloud `sites.list` 回包到达即填表;空列表显示 `(none)`)。
  cloud 侧 `sites.list`/`sites.activate`(bbclaw-reference PR #28)已于 2026-06-12 部署生产。
- **底栏状态条不随 agent 状态变化(空闲/聆听/忙碌/说话/报错无区分)**：chat 模式下 agent 状态
  走 `theme->set_state`,**故意不更新 legacy `s_status`**(`bb_radio_app` PTT release 分支注释
  实证),而底栏 motif 由 `s_status` 驱动 → 永远停在初始态。新增 `bb_display_set_agent_bar_state`,
  在 `buddy_state_listener` / async set_state 处把 agent 状态直接映射到底栏 motif:
  IDLE→breathe / LISTENING→VU / BUSY→sweep / SPEAKING→wave / DIZZY→pulse;chat 退出自动交回
  status 映射。真机实测 PTT 时底栏 mode 随 LISTEN/BUSY 切换(此前一条不变)。
- **(dev 回归)UART 自测任务耗尽内部 RAM 致 adapter WebSocket 任务建不起来、音频流中断**：
  `bb_uart_cmd` 任务用了 6144B **内部** RAM 栈,把内部堆最大连续块挤到 7680B < adapter ws 任务
  所需的 8192B → `Error create websocket task` / `bb_adapter_stream_start failed` → PTT 录音流
  送不出、agent 直接 DIZZY。把该任务栈改用 PSRAM(`xTaskCreateWithCaps`+`MALLOC_CAP_SPIRAM`),
  内部 RAM 零占用。真机实测 ws 恢复连接 + 分片流正常。仅影响 `CONFIG_BBCLAW_DEVICE_MONITOR`
  dev 构建(生产不编入该任务)。

### Changed
- **移除聊天录音遮罩,录音指示改由底栏 VU 跟进——跟手实测 ~240ms→~90ms(最佳 44ms)**：
  chat 主题里那个 320×112 全宽录音遮罩每 48ms 重绘 7 条 meter,是活动态最贵的 LVGL 重绘源;
  而状态机的 dispatch 处理(进 LISTEN/起录音)经 `lv_async_call` 排在 LVGL 渲染之后,被这帧
  ~60–82ms 重绘卡住,连**实际录音起点**都推迟 ~240ms。移除遮罩,录音状态改由 ACTIVE 视图
  底栏(`BAR_LISTEN` / `motif_vu`,canvas 单次 invalidate,便宜得多)表达——它本就按
  `is_recording_status` 独立驱动,功能冗余。`motif_vu` 从合成正弦改为**跟随真实麦克风电平**
  (`bb_display_set_record_level`),保留"被听到"反馈。真机实测重帧 60–82ms→≤34ms 且数量骤减。

### Added
- **LVGL 刷新性能探针(默认关,源码级 opt-in)**：挂 `REFR_START`/`REFR_READY`/`INVALIDATE_AREA`
  事件,渲染持锁超阈值(25ms)即打印耗时+脏区包围盒,定位拖慢 state→render 的重帧来源。
  由 `BBCLAW_LVGL_REFR_PROFILE`(默认 0)控制,**不随生产构建**——故意不挂在
  `CONFIG_BBCLAW_DEVICE_MONITOR` 上(后者在生产 sdkconfig 为 y,会把 heavy-refresh 警告刷到真机)。
  排障时本地手动置 1。

### Fixed
- **PTT 去抖从 ~60ms 收到 ~10ms（对齐 ADR-028 规格，真机实测验证）**：`bb_ptt.c` 原先把
  `BBCLAW_PTT_DEBOUNCE_MS=30` 当**轮询周期**用、再要求 2 个稳定样本 ≈ 60ms，导致 <60ms 的
  快速短按被**静默丢弃**（「按了没反应」），且释放检测滞后 ~60ms（「松手仍在录」）。改为像
  NAV 那样**解耦轮询率与去抖窗口**：新增 `BBCLAW_PTT_POLL_MS=5` 轮询、`BBCLAW_PTT_DEBOUNCE_MS`
  降为 10ms 的去抖窗口（= 2 样本）。真机实测：15ms / 30ms 短按可注册，释放边沿→dispatch
  5ms，15 次按压零丢失零误触。ADR-028 文档写明按键捕获目标 ~10ms，此前实现偏离 6×。

### Changed
- **NAV 上下键 auto-repeat 触发阈值 400→650ms（消除误触连滚）**：`REPEAT_INITIAL_MS` 400ms
  过短，一次稍长的单按就被判成长按并开始 auto-repeat（「长按意外连续滚动」）；650ms 要求明确
  的持续按住。`REPEAT_INTERVAL_MS` 80→100ms（~10 行/秒）。

### Added
- **UART 调试命令注入通道（`bb_uart_cmd`，dev-only）**：在 console UART0 上读 ASCII 行命令
  （`key up|down|left|right|ok|back|ok-long` / `ptt down|up|tap [ms]` / `help`）并注入 nav/PTT
  事件，给只有 UART 桥、TinyUSB CDC1 监控口未接的 bench 板补上**主机驱动按键自测**的闭环：
  写命令→读日志→量响应。配套新增 `bb_ptt_inject()`（带注入保持，poll 期间不与真实 GPIO 抢）。
  gate 在 `CONFIG_BBCLAW_DEVICE_MONITOR`，生产构建不编入。注入点在 GPIO 去抖之上，可验证状态机/
  UI 响应与跟手延迟，但不替代 GPIO 去抖的物理验证。

## [0.5.6] - 2026-06-13

### Added
- **PTT 全状态打断（barge-in，ADR-028 M1+M2）**：PTT 在任何状态按下都生效——立即停止本地
  TTS 播放，并向 adapter 发 `turn.cancel`（local: `POST /v1/agent/cancel`；cloud: WS kind
  `turn.cancel`，云 hub 通用路由直接透传，云端零改动），adapter 杀掉 in-flight `claude -p`
  子进程（SIGTERM→2s 宽限→KILL，连带其中的工具执行一并终止）但**保留 session/resumeID**。
  废除 cloud_wait 期间吞 PTT：取消使 NDJSON/WS 流即刻收尾，设备最长 90s 的阻塞死等自动解除。
- **打断对 `--resume` 可见**：设备随 cancel 上报实际播到的最后一句（playedText，chat 路取
  字幕、voice 路取 chunk tts_text），adapter 记录打断备注并注入下一回合 prompt——恢复后的
  模型明确知道用户听到了多少、执行截断在哪；被打断回合不进长期记忆，auto-title 用原话不受
  注入段污染。
- adapter：`agent.Interrupter` 可选驱动能力 + `butler.InflightRegistry`（进程级 in-flight
  turn 登记，LOCAL/CLOUD 两条链路共用）；被打断回合向设备发 `turn_cancelled` 帧（旧固件
  安全忽略，向后兼容）。

### Changed
- **开机 motor 自检改异步入队，关键路径省 ~500ms**：`bb_motor_init` 原先用
  `vTaskDelay(500)` 直接阻塞 bootstrap 做触觉自检，把后续 audio init / boot wav /
  wifi 全部串行后移。改为把 500ms 脉冲入队给已经创建好的 `motor_task` 并发执行——
  触觉「motor works」反馈不变，但 bootstrap 立即往下走。真机实测：`audio init done`
  提早 ~488ms，`got ip` / `cloud transport ready` 整体提早 ~491ms，boot wav 与并发
  motor 脉冲无干扰（playback ratio 99% / ESP_OK）。
- **状态事件投递从 ~300ms 降到约一个渲染周期（ADR-028 跟手）**：真机二验确认按键反馈/打断
  即时，但 PTT 后的**视觉状态（LISTENING/录音指示）滞后 ~300ms**。根因：TTS 播放 + 转写流
  期间 LVGL 持锁连续渲染（>50ms/帧），状态事件靠抢 LVGL 锁投递，旧的 25ms esp_timer 周期泵
  浅试 10ms 屡屡错过渲染窗口。改为**信号量唤醒的专用 drain 任务**（优先级 7 > LVGL 6、同钉
  core 1）：能阻塞等锁，LVGL 两帧间隙一释放即按优先级拿到锁排空队列，事件在一个渲染周期内送
  达。`dispatch` 首次试锁 50ms→5ms（绝不阻塞 PTT 边沿回调本身），拿不到立即入队由 drain 送。
  注：实际 mic 开录仍受 INMP441/MAX98357A 半双工 I2S 重配约束（~每次切换数十 ms），属硬件
  限制，待 M3 conv_core 一并评估。

### Fixed
- **PTT 打断后设备仍卡 cloud_wait、无法立即重录（真机二验，ADR-028）**：barge-in 时设备
  发出 `turn.cancel` 后仍**阻塞在 `finish_stream` 等服务端回复**（最长 90s，真机实测卡 34s
  直到 WS 被对端 FIN），期间 PTT 只能反复发 cancel、起不了新录音——这是「PTT 能力受限」的
  根因。新增 `BB_WS_EVENT_ABORT` 本地中断位：PTT barge-in 在 `cloud_wait` 时立即唤醒
  finish 等待（`bb_adapter_abort_finish_wait`），**不再依赖服务端是否真的取消**（旧 home
  adapter 不认 `turn.cancel` 时也能跟手）；中断以 `error_code=ABORTED_BY_USER` 收尾，
  radio_app 视为用户主动行为，不弹错误、不震动，stream_task 直接进入用户正在按的新一轮。
- **bb_state 溢出队列泵从未运行致事件全丢（上一版回归）**：排空 timer 惰性创建于「首次
  `dispatch_on_lvgl`」，但开机第一条事件即撞上 LVGL 忙直接进队列、`dispatch_on_lvgl` 从未
  执行 → timer 永不创建 → 队列灌满后 NAV/PTT/FORCE_AGENT 全部 `overflow queue full
  DROPPED`。改为 `bb_state_init` 即建的 `esp_timer` 周期任务（25ms），不依赖任何 LVGL
  上下文，拿不到锁就等下一 tick、事件不丢。
- **真机首验暴露的 4 个跟手问题（2026-06-13 异地日志）**：
  1. *cancel 发不出去*：barge-in 一次性任务用 8KB internal 栈，真机 internal RAM 碎片化
     （largest 常 <8KB）导致 `xTaskCreate` 静默失败——改 `xTaskCreateWithCaps` PSRAM 栈
     并在失败时记 ERROR；
  2. *打断后旧回合 TTS 照播*：`tts_playback_set_active(1)` 开播时清中断标志，洗掉了
     "打断发生在开播之前"的请求——新增 turn 级 stale 标志（voice 路 `s_voice_turn_stale`
     丢 chunk 不播；chat 路 `tts_cancel_requested` 无条件置位 + `tts_kick_or_spawn` 拦截）；
  3. *bb_state 整个会话 net=OFFLINE*：boot 首次 transport ready 路径漏发 `NET_UP`（只有
     心跳"恢复"才发），所有 `PTT_DOWN` 被转移表 `DROPPED reason=net_offline`——boot ready
     时补发；
  4. *LVGL 锁忙时状态事件被丢*（PTT_DOWN/PTT_UP/ASR_RESULT 大量 `lvgl_port_lock timeout
     — DROPPED`）：dispatch 改为锁忙落 PSRAM 溢出队列、由 LVGL 任务内 `lv_timer` 排空
     （保序、不丢），锁等待 200ms→50ms 减少对 esp_timer 任务的拖累。
- **TTS 逐 chunk/逐句 I2S 重配卡顿（ADR-028 M2）**：`bb_audio_set_playback_sample_rate`
  增加幂等保护——目标速率与当前一致时跳过 disable→reconfig→enable 周期（每次 ~50-100ms
  可闻断口）。此前 24kHz TTS 流每个 chunk 都重配一次；agent chat 路更是每句播完回切 16k、
  下句再切 24k，每句两次重配。现在每个 turn 至多一次，回切移到 turn 结束统一做。

## [0.5.5] - 2026-06-13

### Fixed
- **WiFi「最近成功优先」对 compile-time fallback 无效（issue #163 跟进）**：`got_ip` 成功后
  只更新已在 NVS saved slots 的 SSID 的时间戳；连上的 SSID 若不在 slots（典型 compile-time
  fallback `BBCLAW_WIFI_SSID`）则找不到 slot、不记时间戳，永远进不了"最近成功"排序候选，每次
  重启仍按旧 slots 顺序挨个试、fallback 垫底。修：缓存当前连接密码（`s_active_password`），
  `got_ip` 成功时先 `save_sta_credentials` 把连上的 SSID 自动入库，再更新其 `ts=seq`，使其
  下次启动即可优先。

## [0.5.4] - 2026-06-13

### Fixed
- **OTA 升级死循环真根因修复（issue #179）**：设备 cloud_saas OTA 升级曾在按 OK 后 panic
  重启 → 死循环。两处修复缺一不可：(1) **PSRAM cache-freeze** —— OTA 下载烧录原在
  `stream_task`（栈在 PSRAM）同步执行，写 flash 冻结 cache 时触发
  `s_task_stack_is_sane_when_cache_frozen()` assert；改为 `xTaskCreate` 专用 internal-RAM
  栈任务（`ota_apply_task`）。(2) **internal RAM 不足** —— HTTPS 握手 mbedtls IN record
  （16KB）在 internal 导致 `mbedtls_ssl_setup` 返回 `-0x7F00`，且 16KB 任务栈超过 internal
  最大连续块（~15KB）→ xTaskCreate 失败；改 `CONFIG_MBEDTLS_EXTERNAL_MEM_ALLOC`（mbedtls
  堆走 PSRAM）+ OTA 任务栈 16384→12288。真机 v0.5.3→v0.5.4 OTA 升级全链路验证通过。
- **OTA 升级死循环兜底护栏（issue #179）**：cloud_saas 开机 OTA check 新增「重刷循环」
  退避逻辑。设备在切分区重启前把目标版本写入 NVS（`ota/last_try`）；下次开机 check 时
  若 cloud 给的目标版本 == 上次已烧录尝试过的版本，且当前运行版本仍未变成该目标版本，
  判定上一次升级虽烧录成功但固件自报版本号未递增（典型：发布构建用了占位/dev 版本号），
  于是抑制本次 `hasUpdate`、记 `WARN` 日志、正常进主流程，避免无限重刷弹窗。根因仍需
  发布构建注入递增版本号（见 CLAUDE.md 发布约束 / `release_local.sh`），此为软件侧兜底。

### Added
- **设备端切换 Home Adapter（机器）选择器（issue #176, ADR-027）**：cloud_saas 模式下
  Settings 新增 `Adapter` 行 + 选择器，可在已绑定的多台 home adapter 间就地切 active，
  无需打开 Web 后台。列表经 cloud 终结的 WS `sites.list` 异步拉取（Loading→回包重绘），
  选中发 `sites.activate`；离线机器标 `(offline)` 仍可切；切换成功后自动刷新 driver/model
  与 chat session 上下文。仅 cloud_saas 可见，local_home 不渲染该行。

## [0.5.0] - 2026-06-12

里程碑：**一轮完整的固件性能 / 稳定性优化定版**（汇总 0.4.16–0.4.18 的优化，并补齐本地发版工具）。

### 本轮优化总览
- **渲染「不跟手」**：编译 -O2、CPU 240MHz、SPI 40MHz、FreeRTOS 1000Hz、LVGL 刷新
  16ms；Phase 2 代码级（点阵/VU canvas 化、lv_anim 滚动、内置 swap、timer 对齐）；
  消除 core1 上语音管线对 LVGL 渲染任务的抢占。
- **内存**：TTS 流式队列上限 24 + 阻塞背压（长回复不再吃爆 PSRAM、不截断）；
  chat_cache 持久化改静态长驻任务，抗内部 RAM OOM（不再丢对话缓存）。
- **长回复不断连**：home adapter 在 agent 执行期每 15s 补发 reply 流事件，重置云端
  回复空闲计时器；cloud `ReplyIdleWait` 默认 30s→120s 作冗余。修 `HOME_ADAPTER_TIMEOUT`。
- **启动与时钟**：NTP 国内优先（首同步实测 232s→~19s，时钟不再长时间 `--:--`）；
  WiFi 多 SSID 按最近成功时间戳排序；失败重试 3→2，过期 SSID 回退更快。
- **OTA 用户确认制**：开机检测到新版本弹提醒页，OK 升级 / BACK 或超时跳过，不强制。

### Added
- **设备 LED 三色状态灯（issue #166）**：PWM 状态灯用红 / 绿 / 蓝三色展示对话状态。
- **本地发版脚本** `firmware/scripts/release_local.sh` + `make release-local`：本机构建
  固件 + 生成 OTA bundle + 直接推到 OTA 服务器，跳过 GitHub Actions 慢 round-trip
  （也避免本机/CI toolchain 差异卡发版）。复刻 release.yml 的 build→otadata→stage→
  POST /v1/ota/flash-bundle。

### Fixed
- **新 ESP-IDF toolchain（GCC14）下固件编译失败**：`strncpy(dst, samesizebuf, sizeof-1)`
  被 `-Werror=stringop-truncation` 拦下（本地旧 toolchain 不报、CI 红）。3 处改 snprintf，
  并给 src 组件加 `-Wno-error=stringop-truncation` 兜底全部同类点。

## [0.4.18] - 2026-06-11

### Fixed
- **长 agent 回复被 `HOME_ADAPTER_TIMEOUT` 断连、回复丢失**：cloud_saas 语音链路里
  agent（claude-code）跑长工具调用/思考、>30s 无输出时，云端的回复空闲超时
  （`ReplyIdleWait`，滑动窗口）触发 → FIN 断连。25s 连接级 ping 是连接保活、不重置
  per-request 空闲计时器。修复：home adapter 在 agent 执行期每 15s 补发一条
  `voice.reply.status`（reply 流事件，持续重置该计时器；同 phase 云端去重不刷设备
  UI）。配套 cloud 侧默认 `ReplyIdleWait` 30s→120s 作冗余（bbclaw-reference）。
- **新 ESP-IDF toolchain 下固件编译失败**：`strncpy(dst, samesizebuf, sizeof-1)`
  被 GCC14 当 `-Werror=stringop-truncation` 拦下，3 处改 `snprintf`（必空终止）。

### Changed
- **渲染性能 Phase 2（issue #149，代码级）**：在 v0.4.17 配置五连之上做完代码侧优化——
  底栏点阵 + 录音 VU 改 `lv_canvas`，消灭数百个 dot 对象矩阵（#152）；transcript
  auto-scroll 改 `lv_anim` 插值、去掉 tick 内 `update_layout`；`flush_cb` 改用
  esp_lvgl_port 内置字节交换、删除软件 swap（#153）；timer 周期对齐刷新周期、
  `clock_timer` 改局部刷新消除每秒掉帧尖峰（#151/#155）。
- **消除 core1 上语音管线对 LVGL 渲染任务的抢占（issue #149「不跟手」）**：
  `tts_stream_task` 钉 core 0（与 capture/stream 同核），LVGL `task_priority` 4→6
  （高于 stream/tts/ws 的 5、低于 capture 的 7）——渲染任务持锁时不再被抢占，
  PTT/UI 事件等锁超时丢弃大幅减少。
- **OTA 升级改为用户确认制（issue #150）**：开机检测到新版本弹提醒页，OK 升级 /
  BACK 或超时跳过，不再静默强制。

### Fixed
- **线上 OTA 快照一直跑 160MHz**：`sdkconfig.bbclaw.latest`（release.yml 出 OTA 用）
  与 `sdkconfig.defaults.bread` 仍是 Phase 0 老配置，导致 v0.4.17 的 240MHz/-O2/
  1000Hz/16ms 五连从未到达真实用户。两份快照对齐到 Phase 1。
- **TTS 长回复内存暴涨（issue #149/#96）**：流式队列深 128 + 入队超时 0（满即丢），
  长回复（实测 157 块）峰值 queue_depth=88 吃掉 2.67MB PSRAM。改为深度 24 +
  500ms 阻塞背压（满时 WS 接收任务阻塞 → TCP 回压云端放慢发送），长回复语音不再
  被截断、PSRAM 不再被挤爆。
- **chat_cache 持久化在内部 RAM 紧张时丢缓存（issue #96/#146）**：早期每次持久化都
  `calloc` + `xTaskCreate` 内部栈任务，长回复把内部 RAM 挤到 `internal_largest=3840`
  时建任务失败、`s_dirty` 已清 → 缓存永久丢失。改为静态长驻任务（init 预留内部栈，
  运行时零分配、notify 唤醒），永不 OOM。
- **cloud_saas 密语关闭时协调器卡 page=LOCKED（issue #149）**：`s_app_state` 初值
  CHAT 而 bb_state 协调器 boot 默认 LOCKED，开机 `set_radio_app_state(CHAT)` 被
  「状态相同」守卫短路成 no-op → 同步用的 `VOICE_VERIFY_OK` 永不派发 → 每轮语音都
  撞 `INV_2/INV_3` 不变量刷屏。改为密语关闭时显式补一次解锁同步。
- **时钟长时间 `--:--` + 启动慢（issue #149）**：NTP 服务器换国内优先
  （ntp.aliyun.com / ntp.tencent.com / pool.ntp.org），替掉被墙不通的
  time.google.com —— SNTP 首同步从实测 232s → ~19s；`BBCLAW_WIFI_STA_MAX_RETRY`
  3→2，过期已存 SSID 回退快 ~3s。
- **WiFi 多 SSID 接入无优先级（issue #163）**：按最近一次成功连接的时间戳动态排序，
  优先连最近用过的网络。
- **butlermcp 任务状态丢失致 UNKNOWN_TASK（issue #162）**：task 状态持久化到磁盘，
  修复 mcp-server 重启后查不到已派任务。

## [0.4.17] - 2026-06-11

### Changed
- **固件渲染性能 Round 1（issue #149 Phase 1，配置五连）**：LVGL 动画不流畅的主因
  是配置层瓶颈，本版全部拉满：编译优化 Debug(-Og)→PERF(-O2)（LVGL 软渲染快
  30–50%）；CPU 160→240MHz；ST7789 SPI 像素时钟 20→40MHz（320×172 全屏 RGB565
  传输 ~44ms→~22ms，全屏动画硬上限 ~20fps→~40fps）；`FREERTOS_HZ` 100→1000
  （消除 10ms 调度粒度造成的帧间抖动）；`LV_DEF_REFR_PERIOD` 33→16ms（刷新上限
  30→60fps）。注意：SPI 40MHz 在面包板杜邦线环境若出现花屏，回退
  `boards/*/board_config.h` 的 `BBCLAW_ST7789_PCLK_HZ` 即可，其余四项保留。
  诊断、调优准则与实测记录见 issue #149。

### Fixed
- **`bb_ui_task_list.c` 相对时间缓冲可能截断**：`time_buf[12]` 对 `%lld` 格式最坏
  需 18 字节（-O2 的 format-truncation 分析暴露），扩至 20 字节。
- **-O2 下第三方组件 `78__esp-opus-encoder` 编译失败**：GCC 14 在
  `std::vector::insert` 内联展开里误报 stringop-overflow 且被 -Werror 升级为错误；
  根 CMakeLists 对该组件单独 `-Wno-error=stringop-overflow`（不改 managed_components）。

## [0.4.16] - 2026-06-11

### Fixed
- **CHAT 空闲息屏太快（issue #145）**：CHAT 页空闲超时 30s 偏短，且活动计时器只在
  PTT 与 UP/DOWN 按键时重置——LEFT/RIGHT（切 driver）、OK、BACK 都不续命，用户读
  一段长回复就被踢回待机。修复：`BBCLAW_CHAT_IDLE_TIMEOUT_MS` 30s→120s；
  `on_nav_event` 入口统一刷新 `s_last_activity_ms`，六键全部续命。busy/TTS 播放期间
  持续刷新的既有行为不变（倒计时仍从真正 idle 起算）。
- **cloud 纯语音链路丢失 sessionId，息屏重进 CHAT 后对话记录空白（issue #146）**：
  `cloud_saas` 模式下纯语音对话（管家）后，CHAT 空闲退回待机再重进时 transcript 空白——
  缓存重放（ADR-017）、历史分页（Phase S3）、时间分段全部不触发。根因：重进 CHAT 重放
  缓存 + 拉历史的唯一入口被 `s_chat.session_id` 非空守卫，而 cloud 语音链路刻意 flatten
  掉 session 帧，固件语音事件枚举也无 SESSION 类型 → 设备永远拿不到 sid → 不写 NVS、
  `bb_chat_cache` 未绑定 → 重进守卫为假 → 空白。修复：**adapter** 语音（butler）回复路径
  在 turn 起始发新事件 `voice.session`（payload: `sessionId`/`driver`），并在最终
  `voice.reply` 信封冗余携带二者（数据源 butler `EmitSession` 回调）；**固件** 新增
  `BB_FINISH_STREAM_EVENT_SESSION` 事件，`bb_adapter_client.c` WS 分发解析 `voice.session`，
  `bb_radio_app.c` 的 `on_finish_stream_event_tts_only` 经新入口
  `bb_ui_agent_chat_post_session(sid, driver)` 复刻 SESSION 帧持久化逻辑（写 NVS +
  绑定 chat cache + 更新 display），对同一会话内重复 sid 做幂等保护避免清空累积缓存。
  注意：cloud relay 实为 switch-case 重新封包而非透传，需在 bbclaw-reference 补
  `voice.session` 转发（bbclaw-reference#21）并部署后，cloud_saas 模式才端到端生效；
  `local_home` 的 SESSION 帧路径不受影响。

## [0.4.15] - 2026-06-11

### Changed
- **管家 persona 优化对讲机首句即时反馈（issue #140 Phase 1）**：手持对讲机是
  「按下说话、松手等回复」的交互形态，提问后到首句语音之间那段不可预期的静默会割裂
  体验。本期纯 prompt 调优（唯一改动 `adapter/internal/workspace/persona.go` 的
  `DefaultClaudeMD`，零新代码路径 / 零新事件类型，cloud relay 与固件 WS 协议不动）：
  ① 「设备约束」段强化——**第一句必须是一句可立即朗读的简短确认或结论**，不含代码块、
  路径、表格、长列表，展开放到后续句子，让现成的句级流式 TTS 尽早 flush 出声；
  ② 「工作方式」段新增——决定 `dispatch` 长任务**前先用一句话口头确认**（如「好的，我去
  X 项目跑这个任务」），覆盖派发进 running 前的等待。Phase 2 的 filler 占位音旁路
  （仅 dispatch 进 running 时触发）推迟另议。persona 是 adapter 编译常量，随 adapter
  发版生效。

### Fixed
- **管家长期记忆在 cloud-relay 模式下完全不写入（ADR-021 §4）**：记忆写入侧
  (`Memory`)、派活 ring (`DispatchRing`) 与 recorder 此前只接到**本地 ingress** 的
  butler engine 上,**两条 cloud-relay 路径**(`homeadapter` 语音 + agent-proxy)的
  `butler.Deps` 都没接。设备经云端中转时(远程设备的常态),每轮 turn 走的是 cloud 路径
  → `d.Memory==nil` → `RecordTurn` 从不触发 → CLAUDE.md 的 `BBClaw-managed` 收件区
  一直空、`MEMORY/*.md` 一直是模板,派活历史也录不进去。现在 memory/dispatch infra 在
  `run()` 里**只建一次**并同时接到本地 + cloud-relay 两条 engine 上(单 home adapter =
  单 home,共用一个 Writer 是对的;多租户隔离是云端后端的事,不在本仓库)。turn 本身一直
  是成功的(errors=0),所以是纯接线遗漏,不是 turn 失败。
  注:`MEMORY/*.md`(consolidation 产物)仍按设计默认关闭——其两个缺陷未修前,真正生效的
  长期记忆是 CLAUDE.md 收件区;开了 `BBCLAW_BUTLER_MEMORY_CONSOLIDATE` 反而会把收件区
  抽进没人读的单数命名死文件。

### Added
- **派活/长任务 ring buffer 持久化（issue #138 P0 基础）**：`DispatchRing` 此前是
  纯内存的 50 条环形缓冲，adapter 一重启历史就清零——固件 Task List 和 `/admin`
  的「最近派活任务」都变空。现在主进程的 ring 由 `BBCLAW_DATA_DIR/dispatch_ring.json`
  快照支撑（新增 `NewPersistentDispatchRing`）：启动时加载、每次 `Record` 后用
  temp+rename 原子重写（复用 `driverstate.Store` 的落盘范式），缺失/损坏/空文件都
  降级为空 ring 而非启动失败。`GET /v1/butler/dispatch/recent` 的返回 shape
  (`DispatchEntry`) 与 newest-first 顺序保持不变，固件无需改动即向后兼容。这是
  issue #138 的可持久化基础切片；完整 TaskRunStore（per-task `meta.json` +
  append-only `events.jsonl` + Admin 详情页 + 子进程 `task_status`/`task_result`
  接入）按 issue 的开放问题（store 写入方 = 子进程 vs 主进程）拍板后再做。
- **Adapter 管理页「日志」tab + 持久化日志文件（ADR-025）**：新增 `GET /v1/admin/logs`
  （loopback-only）返回内存环形缓冲里最近 ~1000 行运行日志，管理页独立「日志」页实时
  展示（3s 轮询、自动跟随底部、暂停/刷新），用户不必再盯着二进制 stdout。日志同时
  **持久化到 `~/.bbclaw-adapter/adapter-runtime.log`**（即 `$BBCLAW_DATA_DIR/...`，
  任何启动方式都写，超 16MB 滚动到 `.1`，0600），页面顶部显示该绝对路径 + 复制按钮，
  方便 AI/CLI 直接 tail。`obs.Logger` 增加 ring buffer 与 `Tee()` 多路输出（stdout 不变）。
- **设备身份只读展示**：`GET /v1/admin/settings` 增加 `derived` 块（解析后的
  `home_site_id`、构建 `version`、`log_file`）。「设置」页新增只读「设备身份」卡片展示
  这些系统生成值，不再当作可填字段。

### Changed
- **Adapter 管理页重排导航（ADR-025）**：默认落地页从「系统配置」改为
  **「个人对话」**（打开 `/admin` 直接看聊天记录），设置相关页后置。原本独立的
  **「系统配置」「AI 配置」两页合并为单页「设置」**（部署模式 + 设备身份 + 高级设置，
  下接第三方端点 / 项目白名单 / ASR-TTS，顺序排在同一页），顶栏 tab 变为
  **个人对话 / 设置 / 日志 / AI 数据文件** 四项。老书签 `/admin/system` `/admin/ai`
  `/admin/projects` 自动落到「设置」。部署模式保存后会重挂 AI 面板，让本地/云端切换
  即时反映到下方 ASR/TTS 区是否显示。
- **重启开关移到右上角**：去掉原「配置已保存→立即重启」横幅，改为常驻顶栏右上角的
  「重启」按钮；有未生效的已保存改动时按钮高亮 + 脉冲点提示「重启生效」。
- **「设置」页收掉非用户配置项**：删掉 OpenClaw 网关配置区——OpenClaw 与
  claude-code / codex 一样只是一个**驱动**（在「驱动」区出现），不再当默认配置；
  删掉可编辑的 Home Site ID 输入框（改为上方只读展示）；高级设置「云端 relay」精简为
  仅「自建云」WS 地址 + Auth Token（默认指向生产云，开箱即用）。

## [0.4.14] - 2026-06-10

### Added
- **Adapter 配置 Web 化（ADR-025）**：把原本只能写 `.env` 的运行配置（ASR/TTS、
  第三方 Claude 端点、cloud relay、OpenClaw 网关、音频留存）搬上本地管理页，落
  `~/.bbclaw-adapter/settings.json`（`.env` 退化为首启一次性 seed，之后页面是真相）。
  管理页拆成四页：**系统配置 / AI 配置 / 个人对话 / AI 数据文件**。改动「保存即持久化 +
  一键重启生效」（`POST /v1/admin/restart` 原地 re-exec）；驱动/模型/项目仍即时生效。
- **默认 cloud、本地免配语音**：本地语音管线（LAN 直连 ASR/TTS）改为 opt-in
  (`topology.local_voice_enabled` / `BBCLAW_LOCAL_VOICE`)。云端模式下 ASR/TTS 由云端
  完成，本地空 `.env` 即可启动；关闭时 `/v1/stream/*`、`/v1/tts/*` 回 501。既有完整
  语音配置自动保留（首启按 env 是否完整自动判定）。新增 `GET/PUT /v1/admin/settings`
  （loopback-only，明文读写）。
- **管理页按部署模式自适应简化**：系统配置页改为单选「☁ 云端 / ⌂ 本地」一个选择驱动
  全页——云端模式本地零配置（cloud relay / OpenClaw / 音频留存收进「高级设置」折叠），
  本地模式才在 AI 配置页显示 ASR/TTS。语音未配完整时**优雅降级**（保存 200 +
  `voice_incomplete` 提示、启动 WARN + 语音回 501）而非报错/崩启动，避免「切本地模式
  才出 ASR 表单、但没填 ASR 又切不过去」的死锁。

### Changed
- **底栏扫描条改为 3×N 点阵带**：单排扫描条升级为 3 行点阵小屏（dot 4 / pitch 7 /
  vpitch 5），彗星变成"整列"白头青尾的全高扫描，更像一块迷你点阵屏；颜色/速度随状态
  联动不变。
- **底栏点阵带按状态切 motif**：不再只有单一 sweep，改为随对话状态切换不同动画——
  处理=扫描 sweep / 聆听=声波 vu / 说话=行波 wave / 错误=红脉冲 pulse / 待机=呼吸
  breathe。与官网 daboluo.cc/style 的 `dot-matrix-anim.js` motif 库同源。
- **状态栏精简 + 电池可读性**：顶栏去掉占地的 WiFi SSID 文字（信号格已表达"有 WiFi +
  强度"），腾出的空间让电池重新露出；电池常态填充改冷白（更像标准电池，充电绿/低电红
  不变），并在图标左侧加 `NN%` 数字（低电红、充电绿）。

### Fixed
- **adapter release CI 因 prewarm 测试清理竞态失败**：加目录触发的 `prewarm.RecordAsync`
  异步写 `MEMORY/projects.md`，会在测试 `t.TempDir` 清理后落盘 → `directory not empty`
  让 `TestAdminProjectsAddListDelete` 失败、挡住发版。给 prewarm 加 `WaitGroup` + `Wait()`，
  测试 helper 在临时目录拆除前 await 在途扫描。

### Removed
- **移除对话页右上角字符小人（buddy）**：agent 聊天主题 `bb_theme_buddy_anim` 过去在
  transcript 右上角浮一个 `(^_^)` 字符表情 + mood 小窗（九态动效）。角色状态已由顶部
  状态栏图标 + 底栏点阵扫描条表达，右上角小人冗余且遮挡正文，故移除。transcript 聊天
  消息流、录音遮罩、历史回放等主题功能保持不变；`set_state` 仍保留（驱动顶栏图标语义）。

## [0.4.13] - 2026-06-10

### Changed
- **WiFi 配网改为独立全屏页**：SoftAP 配网模式过去把 AP 的 SSID/密码/IP 塞进一个
  对话气泡（`bb_display_show_chat_turn`）显示，拥挤且与聊天内容混淆。改为独立的点阵
  配网页 `bb_page_apconfig`：左侧点阵 WiFi 广播图标（向外涟漪），右侧"热点/密码/打开"
  三步加入指引（CJK 文字渲染）。首启无凭据和运行中 WiFi 掉线两条进入路径都已切换。
  详见 design/STATE_MACHINE.md §3.5.2。
- **对话页底栏改为点阵扫描条**：ACTIVE 对话页底栏过去是 `[B] cwd | mem:N+M` 文字双格，
  现改为一条 320px 点阵"小屏幕"——白头青尾的彗星在一排 ghost 点上 L↔R 往返扫描
  （Knight-rider），颜色与速度随状态联动：待机青慢 / 聆听青中 / 处理青快 / 错误红
  （NO WIFI、WIFI ERR、AUTH）。cwd / mem 统计改由锁屏页 footer 承载，派发进度仍叠加
  在顶栏状态文字；记忆/cwd 状态与 API 不变。详见 design/UI_DESIGN_LANGUAGE.md §3。
  模拟器 headless 导出新增逐帧推进（`lv_tick_inc`），动画类预览不再停在 t=0。

## [0.4.12] - 2026-06-10

### Changed
- OTA 验证版本:相对 v0.4.11 无代码变更,仅用于在真机上走一遍修复后的完整 OTA
  链路(下载 → 进度页 → 重启 → 自报新版本 → device_id 稳定不丢配对 → 收敛)。

## [0.4.11] - 2026-06-10

### Fixed
- **OTA 升级后设备变"新设备"**: `device_id` 原为 `BBClaw-<固件版本>-<MAC>`,版本号从
  tag 注入后,每次 OTA 都会改变 `device_id` → 云端当作全新设备要求重新配对
  (`claim_required`、配置回默认)。改为 `BBClaw-<MAC>`,与固件版本无关、跨 OTA 稳定。
  一次性影响:现有已配对设备的 id 会变一次,需重新认领一次,此后永久稳定。(#123)

## [0.4.10] - 2026-06-10

### Fixed
- **OTA 无限重刷循环**: 固件版本(`esp_app_desc.version`)此前硬编码在
  `CMakeLists.txt` 的 `project(... VERSION 0.4.1)`,OTA 后设备永远自报 0.4.1 <
  云端 active → 反复重新下载同一版本。改为版本从 `version.txt`(CI 写入发布 tag)
  / `git describe`(本地)注入。(#122)
  *(v0.4.9 的 octal PSRAM 修复已在真机验证:能正常启动,仅版本号循环。)*

## [0.4.9] - 2026-06-10

### Fixed
- **OTA 变砖修复(关键)**: 发布固件改用 `firmware/sdkconfig.bbclaw.latest`(OCTAL PSRAM
  + bbclaw 板 + cloud_saas + 生产云 URL)构建。此前 CI 走 `sdkconfig.defaults`(QUAD PSRAM
  + breadboard + local_home),发的固件 OTA 到八线 PSRAM 的 bbclaw PCB 会
  `wrong PSRAM line mode` → `Failed to init external RAM` → boot loop。OTA 链路本周才修通,
  v0.4.8 是第一个真正被 OTA 的 CI 构建,因此首次暴露。(#120 #121;默认板也改为 bbclaw)

### Added
- **开机动画下方显示当前固件版本**(`bb_ota_get_current_version`),便于确认实际启动的
  构建 / OTA 分区。(#119)

## [0.4.8] - 2026-06-10

### Added
- **设备端音量调节（固件 Settings）**：Settings 新增 `Volume` 行 + 调节态，UP/DOWN
  ±5% 实时生效、点阵进度条，OK/BACK 保存返回（#112/#113）。开机应用已保存音量、云端
  心跳不再在开机/同值时回灌覆盖本地选择（#114）。
- **语音调节设备音量**：管家 agent 经 `bbclaw-adapter device set-volume <pct> --device <id>`
  CLI 调云端写配置 → 下发设备;每轮 butler sysprompt 注入当前 deviceId + CLI 用法（#116/#117）。
  *(云端 `POST /v1/devices/{id}/config` 接受 home-adapter Bearer + camelCase + 双 store 下发，
  server-side 部署。)*
- **OTA 升级点阵进度页**（`bb_page_ota`）：下载时屏幕显示点阵进度条 + `UPDATING NN%` +
  目标版本，完成显 `REBOOTING`，替换原静默下载（#118）。
- **固件历史按会话段展示**：解析 adapter 下发的 `timestamp`，设备端历史按时间分段渲染;
  离线时提示「仅本地缓存」（#110/#111）。

### Fixed
- **设置里调音量/切 TTS 按 OK 必崩重启**：NVS 写发生在 PSRAM 栈的 stream_task 上，写
  Flash 冻结 cache → assert 重启;改为投递到内部 RAM 栈任务持久化（#115）。
- **自动 OTA 链路断裂（云端）**：release workflow 只传 flash-bundle（bundle store），但
  `/v1/ota/check` 读 firmwares store，从不联动 → 永远无更新。flash-bundle 上传时同时把 app
  镜像注册为 active OTA firmware;`ParseVersion` 容忍 `v` 前缀（server-side 部署）。

## [0.4.7] - 2026-06-09

### Added
- **管理页改为独立 Vue SPA（`adapter/web`）+ 对话记录页**：把原内嵌 vanilla 单页重写为
  Vue3+Vite+TS 工程，构建产物 `internal/adminui/dist` 提交进仓库并 `go:embed` 打进
  二进制 serve `/admin`（**单二进制不变、发布流水线零改动、不需要 Node**；`make web`
  仅在改前端时重建）。新增**对话记录**标签页：会话列表 + 消息气泡流（user/assistant）
  + 派活任务卡片，**按对话时间间隔自动分段**（>30min 插入时间分割线）；消息接口补
  `timestamp` 字段（`agent.Message`，从 claude transcript 解析）。参考 agent_room 的
  记录展示模式，沿用点阵风格。新增 localhost-only 只读会话接口 `/v1/admin/sessions[/{id}/messages]`、
  `/v1/admin/dispatch/recent`。原 `internal/httpapi/admin.html` 退役。
- **管理页升级：工作区文件预览 + 目录关键字搜索 + 多选批量加入 + 点阵风格**：
  `/admin` 现可**只读预览**管家工作区文件（`CLAUDE.md` 人设 + `MEMORY/{profile,
  preferences,projects,decisions}.md`，白名单防任意读，新增 `/v1/admin/workspace-file[s]`）；
  目录选择器加**关键字搜索**（过滤当前目录 + 服务端递归搜索 `/v1/admin/fs/search`，
  限深限量、跳过 `node_modules`/`.git` 等）与**多选批量加入**；整页重做为**点阵 /
  Nothing-style** 视觉（对齐 `design/UI_DESIGN_LANGUAGE.md` token）。
- **新增 Claude 技能 `dot-matrix-ui`**（`.claude/skills/dot-matrix-ui/SKILL.md`）：把点阵
  设计语言提炼为可复用的 Web 落地版（token、CSS 变量、组件配方、do/don't），供 adapter
  页面与 daboluo.cc 官网统一复用，与固件 `bb_ui_theme.h` 同源。
- **启动后自动打开管理页**：本地 HTTP 起来后自动用默认浏览器打开 `/admin`（跨平台
  open/xdg-open/start，headless 失败静默非致命，`BBCLAW_OPEN_ADMIN=0` 可关）。
- **管理页改为目录选择器**：浏览器拿不到本地绝对路径，故新增服务端目录浏览
  `GET /v1/admin/fs?path=`（仅 localhost）逐级浏览主机目录来选项目目录；项目**名称
  从目录名自动派生**（去重加 `-N` 后缀），管理页不再要求手填名称（`store.AddPath`）。
- **ASR 项目名识别准确性（两层）**：① 管家层——persona 指令让 LLM 先 `list_projects`、
  再按读音/拼写把用户语音里的项目名模糊匹配到真实项目再 dispatch（provider 无关、
  主防线）；② ASR 层——`asr.Metadata.Hotwords` 注入实时项目名，OpenAI/Whisper provider
  作为 `prompt` 偏置下发（Doubao bigmodel 需预注册 boosting table，暂为文档化 no-op）。
- **本地轻量 Web 管理页（`/admin`）+ 运行时项目目录管理（web-first）**：adapter 启动后在
  `/admin` 提供一个零依赖单页（embed，无构建步骤），展示运行状态（健康 / 本地服务 /
  已注册驱动，只读）并支持**增删管家可派活的项目目录**。新增 `internal/projectstore`
  持久化项目池（`<DataDir>/projects.json` 为**唯一真相源**，原子写、mtime 变化时重读），
  主进程与 `mcp-server` 子进程共享同一文件 → 加目录**无需重启**即对管家派活生效。
  **`BBCLAW_CWD_POOL` 改为一次性 bootstrap 播种**：首次运行写入文件后即被忽略，之后
  所有项目（含原 env 项目）都在 web 页可增删；旧版 `{"added":...}` 文件会在启动时
  自动迁移为新格式并把 env 项目并入，**不丢已添加的项目**，迁移后即可从 `.env` 删掉
  `BBCLAW_CWD_POOL`。加目录后异步轻量扫描仓库（语言栈 / README / 近期 git 提交）
  生成摘要写进 `MEMORY/projects.md`，**预热**管家上下文（`internal/prewarm`）。
  安全：`/admin` 与 `/v1/admin/*` **仅限 localhost**（按对端地址判定 loopback，与设备
  auth token 解耦），因为加目录等于授予管家在该目录跑命令/文件执行的权限。
- **管家首次激活的对话式身份初始化（onboarding）**：新增 `MEMORY/profile.md`
  身份档案维度（怎么称呼用户 / 角色 / 职业），并在管家启动人设
  （`workspace/CLAUDE.md`）注入初始化指令——`STATUS: uninitialized` 时管家先办
  用户当下请求、再顺势用一两句话录入身份，写入后置 `initialized`，用户拒绝则
  `skipped`，绝不打断紧急任务。身份档案由管家手写维护，**不进**自动蒸馏 / 整理
  循环，避免被提炼笔记覆盖（`internal/workspace/`）。
- **adapter 独立介绍文档** [`adapter/docs/butler.md`](adapter/docs/butler.md)：管家
  工作空间模式说明、一键复制给 AI agent 创建并启动、参考配置示例、初始化对话流程；
  另附官网 Adapter 页面内容草稿 `adapter/docs/website-adapter-page.md`。README 顶部
  加入指引链接。

## [0.4.6] - 2026-06-07

### Changed
- **全 UI 统一点阵设计语言（dot-matrix / Nothing-style，design/UI_DESIGN_LANGUAGE.md）**：开机动画 / 网络连接页 / 待机时钟三页确立的视觉语言推广到全机。新建唯一真相 token 表 `design/UI_DESIGN_LANGUAGE.md` + `firmware/include/bb_ui_theme.h`，11 个 UI 源文件的本地调色板全部收敛为 token 引用，禁止裸 hex 色。关键视觉变化：
  - **底色统一**：旧 `0x0a0e0c`（偏亮）全部换 `BB_UI_BG 0x070b0e`——页面切换不再有底色跳变；状态栏/底栏暖绿 `0x8fbcac` 与旧暖灰次级文字统一为冷蓝灰 `BB_UI_TEXT_DIM 0x6e8a93`
  - **LOCKED 页点阵重构**：锁形改点阵画法（7 点 shackle 弧 + 5×4 点 body + 青色 keyhole 呼吸点），密语验证时呼吸加速、失败时 body 闪红一拍；顺手修复旧布局 title y=208 / hint y=230 在 172px 屏上**越界不可见**的 bug——文案移到锁形右侧块内
  - **录音 VU 点阵化**：chat 录音遮罩（7×5）与语音 speaking 视图（10×5）的连续条 VU 全部改为点阵柱，bottom-up 点亮、峰值点 voiced 时青色闪——与开机扫列同节拍语言；平滑攻衰减逻辑保留
  - **聊天气泡单色化**：assistant 蓝色 `0x4a9fd8` 弃用，改 ghost 深灰面 + 冷白文字；用户气泡保留青色 30%——左右对齐 + 色块强弱区分说话者
  - **buddy 九态全收敛单色+青**：face 暖奶油/mood 暖棕/attention 金黄/celebrate 粉全部映射到冷白/冷蓝灰/青三色，状态差异交给既有动效表达
  - **列表选中态统一**：Settings / 任务列表 / chat picker 三处选中行统一为「ghost 行面 + 青色左缘 3px 竖条 + 冷白文字」（替换旧的青底白字/青底黑字混用）
  - 模拟器新增 `--mode locked`（支持 `--status "VERIFY ERR"` 等预览验证态）

## [0.4.5] - 2026-06-07

### Removed
- **设备端 CWD picker + new-session 死代码整链清理**：ADR-021 v2 砍掉 session picker 后，CWD picker（issue #30）唯一入口（session picker 的「+ 新建 session」行）随之断链，整条链路不可达：CWD picker overlay UI 与按键路由、设备端 new-session worker（`spawn_new_session_task` 等）、客户端 API `bb_agent_create_session` / `bb_agent_list_cwd_pool`（`POST /v1/agent/sessions` / `GET /v1/agent/cwd-pool` 设备侧调用）、`bb_display_set_cwd_name` 及 `format_relative_time`。设备 turn 已由 adapter `EnsureButler` 强制路由管家会话，设备端不再创建 session。共删 ~650 行（6 个源文件 + 3 个头文件）。

### Fixed
- **开机动画没播完就被硬切**：splash dismiss 只看墙钟 `BBCLAW_BOOT_SPLASH_MIN_MS`（2600ms），但扫列动画跑在 LVGL task 的 `lv_timer` 上，boot 期间音频 init / boot wav 播放会饿 LVGL task，节拍落后墙钟 → 到点时列没扫完/下划线没长完就被销毁。修复：新增 `bb_page_boot_anim_done()`（收尾 tick 自删 timer 即 done），`bb_radio_app_start` 在 MIN_MS 补足后继续 50ms 轮询等动画真正收尾，上限 `BBCLAW_BOOT_SPLASH_ANIM_GRACE_MS`（默认 2000ms）防 LVGL 卡死无限等。设计文档 `STATE_MACHINE.md` §3.5 同步。
- **待机时钟 SNTP 同步前全黑**：`bb_page_standby_refresh_clock` 对 `"--:--"`（时间未就绪）解析不到数字 → 4 个 slot 全 ghost ≈ 黑屏。兜底：无数字时各 slot 渲染居中横杠（5×7 中间行 3 点亮），任何 fallback 路径下时钟页都有内容。
- **麦克风近场削顶导致云端 ASR 识别为空**：INMP441 软件增益 8x 对近场/稍大声说话把波形顶到 `INT16_MAX`（pcm diag `max=32767 clipped=17`、正向严重削顶、负偏分布），失真音频上行后云端 ASR 返回空文本（`phase=asr text= (empty)` → `agent_chat: empty transcript`）。`BBCLAW_AUDIO_INMP441_GAIN_NUM` 8→4，保留 ~2 bit 动态余量同时对 INMP441 偏低的原始电平仍够响。
- **失效逻辑会话每次开机刷 `SESSION_NOT_FOUND`**：设备重启后从 NVS 复用上次逻辑会话 id，若该会话已在 adapter/cloud 侧失效，拉历史返回 HTTP 400 `SESSION_NOT_FOUND`，旧逻辑当通用失败处理、本地 id 永不清除 → 每次开机复现、历史区常空。修复：`bb_agent_load_messages` 对 `SESSION_NOT_FOUND` 返回独立的 `ESP_ERR_NOT_FOUND`，`on_history_fetch_done` 据此自愈——清 NVS 会话（`bb_session_store_save(drv, "")`）+ 重置内存 `session_id` + 清残留 transcript/cache，下个 turn 由 adapter 新建会话并自动存回 NVS。
- **运行时内部 RAM 耗尽 → websocket task 创建失败、语音流挂**（`Error create websocket task`，internal_free=27KB / largest=7.6KB < 8KB task 栈）：bbclaw 板配置用 CLIB malloc + `SPIRAM_MALLOC_ALWAYSINTERNAL=16384`，每个小 lv_obj 都落内部 RAM——点阵 UI 风格（待机页 140+ dots、开机动画 210+ dots、聊天气泡）数百个小对象把内部堆吃碎。修复双管齐下：(1) 新增 `bb_lvgl_mem.c` 自定义 LVGL 分配器（`CONFIG_LV_USE_CUSTOM_MALLOC`），所有 LVGL 分配 PSRAM 优先、内部兜底，内部 RAM 留给 task 栈和 WiFi/I2S DMA；(2) `SPIRAM_MALLOC_RESERVE_INTERNAL` 32K→64K，可去 PSRAM 的 malloc 更早转移。`sdkconfig.defaults` / `sdkconfig` / `sdkconfig.bbclaw.latest` 三处同步（顺带修正 committed sdkconfig 与 defaults 的 BUILTIN/CLIB 漂移）。
- **开机动画淡出与 WiFi init 并发导致 ESP_ERR_NO_MEM boot loop**：splash dismiss 原为 350ms 整屏 opa 淡出（异步）——parent-opa 让 LVGL 经临时全屏合成层渲染，且淡出窗口与 `esp_wifi_init` 重叠，WiFi 的 10×1600B 静态 RX DMA 缓冲拿不到内部 RAM（`wifi:malloc buffer fail` → `Expected to init 10 rx buffer, actual is 5`）→ `ESP_ERROR_CHECK` abort 重启循环。修复：dismiss 改为同步硬切销毁（splash 与待机/锁屏底色几乎同色，视觉无碍），返回即全部资源释放；另在 WiFi init 前加 `log_heap_snapshot("pre_wifi_init")` 水位快照便于复现定位。
- **buddy 九态表情动画全部失效（Phase 7 死代码复活）**：Phase 7 把 chat overlay 改透明时移除了主题自有 topbar，face/mood label 置 NULL 但注释声称"已迁到 ACTIVE 视图"——实际从未迁移，`apply_state_anim()` 等 ~300 行动画代码因宿主对象为 NULL 永远 early-return，任何按键/PTT 引发的状态切换都无表情动画。修复：在 transcript 右上角重建 ~96×38 半透明圆角 buddy chip（face+mood 两行），九态动画（sleep 呼吸/idle 浮动/busy 点点/speaking 摇摆/heart 心跳/listening 浮动+脉冲/dizzy 抖动/attention 变色/celebrate 弹跳）整体复活；录音遮罩 show 时仍 move_foreground 盖住 buddy 避免 LISTENING 双重提示。设计文档 `STATE_MACHINE.md` §3.1/§3.3 同步。
- **回复语音没播完就被切到待机页**：CHAT→STANDBY 的 30s 空闲判定只看 voice-PTT 管线的 `s_tts_playback_active`，而 agent 回复朗读走的是 `bb_ui_agent_chat.c` 里独立的 `tts_playback_task`（turn 结束 busy=0 后仍逐句合成+播放，喇叭输出显著滞后于 turn 生命周期），长回复播到一半就被 idle 定时器拉去待机页。修复：空闲判定改为「设备空闲 + 喇叭空闲」三重检查——`s_tts_playback_active`（语音管线）+ 新增 `bb_ui_agent_chat_tts_speaking()`（chat 朗读任务存活，含合成等待期）+ `bb_audio_is_playback_active()`（I2S TX 通道电平，兜底提示音等）。喇叭活跃期间持续刷新活动时间戳，待机倒计时从**语音完整结束**那一刻起算。
- **待机页直接按 PTT 无「正在聆听」动画/震动反馈**：`stream_task` 的 STANDBY→CHAT 唤醒分支先消费了 PTT 版本号再 `continue`，下一轮循环只剩 `s_ptt_pressed` 电平驱动的 arm 录音路径，唯一触发 LISTENING 状态 + 录音波形遮罩 + 按下震动的 `chat_voice` 边沿分支被整个跳过——录音在跑但屏幕毫无反馈。修复：`agent_chat_enter()` 成功后不再吞边沿，落穿到同一迭代的 `chat_voice` 分支，行为与已在 CHAT 页按 PTT 完全一致（含 busy/adapter-offline 防护）。
- **固件编译失败**：(1) `bb_ui_agent_chat.c` 一处旧注释缺 `*/` 结尾，与下一行注释嵌套触发 `-Werror=comment`；(2) `bb_lvgl_display.c` / `bb_page_locked.c` 底栏 `"mem: %d+%d"` 在 int 极值下可能截断 24 字节 buffer 触发 `-Werror=format-truncation`，inbox/profile 显示值钳制到 999。

### Added
- **网络连接点阵动画页 (`bb_page_netconn.c`, STATE_MACHINE.md §3.5.1)**：待机页是点阵时钟，SNTP 同步前没有内容可显示，而 WiFi 连接最长 30s+/SSID——开机动画硬切后用户面对近乎黑屏。新增网络连接页无缝接管：同点阵语言（5px dot / 同 palette）的 WiFi 弧形图标（底部基点 + 3 层同心点弧，共 16 dots）自下而上逐层点亮循环（420ms/层，最新层青色闪、下一拍沉淀冷白），图标下方实时显示正在尝试的 SSID（`WiFi <ssid>`，每 tick 轮询 `bb_wifi_get_active_ssid()`，多 slot 重试时自动跟随）；连上后弧全亮定格青色、标签换 `SYNC TIME`，等到 `bb_wall_time_ready()`（或连上后超时 `BBCLAW_NETCONN_SYNC_TIMEOUT_MS`，默认 10s）自销毁，露出**已有时间**的待机时钟。provisioning / wifi 失败路径由 `bb_radio_app_start` 显式 dismiss 让位 AP info / 错误显示。show/dismiss 与 splash 同样同步硬切不做 fade（NO_MEM 教训）。开关 `BBCLAW_NETCONN_PAGE_ENABLE`（默认 1）。
- **诺基亚式像素点阵开机动画 + 语音协同 (`bb_page_boot.c`)**：开机后在 `lv_layer_top()` 全屏深色底铺出 "BBCLAW" 六字母 5×7 ghost 点阵（复用待机页点阵语言：dot 5px / pitch 9px），逐列扫亮（35ms/列，最新列青色高亮、下一拍沉淀冷白），扫完青色下划线从左向右生长收尾，结束整体淡出露出底层视图。开机语音（boot wav）延迟到扫列完成后才播：`bb_radio_app_start` 在 SPK TEST 前等到动画开始 ≥`BBCLAW_BOOT_SPLASH_VOICE_DELAY_MS`（默认 1150ms），播完后不足 `BBCLAW_BOOT_SPLASH_MIN_MS`（默认 2600ms）补足再淡出。开关 `BBCLAW_BOOT_SPLASH_ENABLE`（默认 1）。设计文档 `STATE_MACHINE.md` §3.5。
- **管家记忆「沉淀引擎」— 收件箱归档进 MEMORY 多维画像并清空 (ADR-022 v1, #92)**：在 ADR-021 §4 per-turn distill（收件箱 append）之上新增**第二层「沉淀」**：后台把 workspace `CLAUDE.md` 托管段（收件箱）归档进 `MEMORY/*.md` 多维画像并清空收件箱，使 4KB 从「FIFO 静默丢失硬上限」降级为「整理缓冲」。默认 **off** 灰度、LOCAL-only。
  - **触发器（四类 + cooldown，`trigger.go`）**：阈值（收件箱 ≥75% `maxBytes`）/ 空闲（`idleGap`，默认 5min）/ 兜底（`maxGap`，默认 6h）/ per-key cooldown（默认 10min）。决策是纯函数 `decideTrigger`（cooldown 门 → 阈值 → 空闲 → 兜底，空收件箱永不触发），I/O 与决策分离便于表驱动测试。挂在 `MemoryWriter.RecordTurn`（engine.go:538）打 turn 末时间戳（单次轻量赋值，保持非阻塞契约）；阈值在 worker 每轮 append 后同步检查，空闲/兜底由轻量 ticker 周期驱动。
  - **整理引擎（`consolidator.go`）**：读收件箱（`ManagedBlock` 快照）+ 现有 `MEMORY/*.md` → `claude -p` Haiku 归类成 JSON 对象（preference/project/decision 三维度）→ 经 `IsPoisoned` **双过滤** + 每维度上限（默认 30）裁剪 → **0600 原子写**各 `MEMORY/<dim>.md`（无绝对路径，目录防御式自建 `filepath.Dir(CLAUDE.md)/MEMORY`，与 #91 解耦）→ **仅全部写盘成功才清空收件箱**；任一步失败全吞(log)、**绝不清空**（下轮从仍在的收件箱重derive，收敛）。
  - **整理 prompt 规则（`summarizer_claude.go`）**：合并 / 去重 / **新覆盖旧（真删）**（每次对 `MEMORY/<dim>.md` 整文件重写，被取代的旧事实物理删除）/ 剔除过期 / 丢指令性内容 / 每维度上限。
  - **读-清空与 append 竞态**：清空采用**快照感知清除**——只移除「快照集合里出现过的行」，沉淀期间（LLM 调用秒级）新 append 进收件箱的行不在快照内、予以保留。即使未来 consolidation 移出 worker 并发执行仍正确。复用同一**并发=1 worker**：distill append 与 consolidation 在同一 worker 串行排队（distill 走 `ch`，consolidation 走独立信号 + worker `select`，ticker 满即丢不互相饿死）。
  - **spike 结论**：`claude -p` Haiku 大 JSON 稳定性采用与 `parseItems` 同源的**容错切片**（首 `{` 到末 `}`）而非 `--output-format json`（后者多一层信封）；解析失败 → 不清空收件箱（下轮重试），脏条目 → `IsPoisoned` 拦截 + 每维度上限封顶。本仓 CI 无 claude CLI/API key，真链路冒烟留集成测试（`BBCLAW_BUTLER_LIVE`）。
  - **env 门控**（沿用 `BBCLAW_BUTLER_MEMORY_*` 约定）：`BBCLAW_BUTLER_MEMORY_CONSOLIDATE`（默认 off，需 `_DISTILL` 也开）/ `_CONSOLIDATE_THRESHOLD`（0.75）/ `_CONSOLIDATE_IDLE`（5m）/ `_CONSOLIDATE_MAXGAP`（6h）/ `_CONSOLIDATE_COOLDOWN`（10m）/ `_CONSOLIDATE_MAXPERDIM`（30）。模型/二进制复用 `_MODEL` / `_CLAUDE_BIN`。
  - **单测**：四类触发表驱动（阈值/空闲/兜底/cooldown/首轮）；整理引擎写盘+清空 / 0600 / `IsPoisoned` 双过滤 / 每维度上限 / 空收件箱 no-op / summarize 失败保留收件箱 / **读-清空 vs 后到 append 竞态** / 二次幂等 / JSON 容错切片；worker 集成（RecordTurn 触发阈值沉淀跑在同一 worker）。顺带修正 63e80fc「默认 ON」遗留的两处 `config_test.go` 失效断言。
- **管家长期记忆 — turn 末蒸馏要点 append 进 workspace CLAUDE.md (ADR-021 §4 v1, #83)**：给「管家」(`RoleButler`) 会话加持久长期记忆，让管家 `claude -p --resume`(cwd=workspace) 重启/换机后仍记得用户偏好与在做的项目。
  - **写入机制（engine 内部步骤，不新增 caller hook）**：`butler.Engine` 收尾点在【`Role==RoleButler && turnEnded && errorCount==0`】时，经新增窄接口 `Deps.MemoryWriter.RecordTurn(userText, replyText, cwd)` **非阻塞**投递本轮。选 engine 内部步骤而非 `Hooks.OnTurnComplete`：通知/reply 路径都不带 `req.Text`(ADR-020 §4)，且蒸馏对 LOCAL/CLOUD 完全相同，不该按 caller 注入。`MemoryWriter==nil`(默认) 整步跳过。
  - **记忆落点（唯一）**：workspace `CLAUDE.md` 的 `<!-- BEGIN/END BBClaw-managed -->` 托管段，复用 `workspace.ReplaceManagedBlock`。**砍掉 ADR-020 的 `memory.json` 注入层**（§1/§2/§4 在管家模式下 Superseded by ADR-021）；各项目 cwd 的 CLAUDE.md 项目画像仍是独立轴。
  - **蒸馏管线（`internal/butler/memory`）**：后台**单 worker(并发=1)** 起 Haiku `claude -p` 把本轮蒸馏成 JSON delta（用户长期偏好 / 最近项目 / 关键决策三类）→ **deny 过滤**（含 `ignore previous`/`system prompt`/`bypass`/`你现在是…` 等指令式条目整条丢，防注入持久化）→ **hash 去重**（幂等）→ **≤4KB FIFO clamp**（防膨胀）→ **原子写 0600**。门控：跳过错误轮 / 过短 utterance / 队列满即丢。失败全吞(log)，绝不阻塞 turn 返回设备。
  - **安全分级**：env `BBCLAW_BUTLER_MEMORY_DISTILL` 默认 **off**（链路 smoke 前不写）；LOCAL 灰度开；**cloud 多租户 v1 不注入写入**（user 维度落地前避免串写）。`BBCLAW_BUTLER_MEMORY_MODEL` / `BBCLAW_BUTLER_MEMORY_CLAUDE_BIN` 可覆盖模型与二进制。
  - **单测**：marker splice append / hash 去重幂等 / ≤4KB FIFO clamp / deny 过滤命中 / 0600 / 原子写无副作用；engine 投递门控（管家 vs 非管家 vs 错误轮 vs nil）/ writer 非阻塞满即丢 / 过短跳过 / env 默认 off。Haiku 真链路属外部 CLI 依赖，留集成冒烟。
- **管家会话路由 — 设备 turn 永远路由到 workspace 管家会话 + `--mcp-config` + WarmPool 预热 (ADR-021 v1, #80)**：设备每次语音/文本 turn 不再自选 driver/session，统一路由到该设备专属的「管家」逻辑会话（`Role=butler`、`cwd=~/.bbclaw-adapter/workspace/`、`driver=claude-code`），管家靠 cwd 自动加载 workspace 的 CLAUDE.md 人设/记忆。
  - **管家会话解析**（`logicalsession.Manager.EnsureButler`）：按 `deviceID+driver` 幂等解析/创建管家会话；首次铸造 `RoleButler`，后续复用以保留会话连续性。每设备一个管家。
  - **路由落点**：`httpapi/agent.go handleAgentMessage`（local）与 `homeadapter/adapter.go handleChatTextViaAgent`（cloud 语音）在配置了 butler workspace 时，忽略设备请求的 driver/session，改喂管家会话走 `butler.Engine.RunTurn`。语音路径从「手撸 `drv.Start`/事件循环」统一到 butler 引擎，经 `voiceEventSink` 适配回 `voice.reply.delta`/`tool_call` 帧，云端协议帧序列不变。未配置 workspace 时（如单测）保持旧多会话行为不破。
  - **`--mcp-config`（仅管家）**：`agent.StartOpts` 新增 `MCPConfig` 字段（契约同 `Model`/`SystemPrompt`，不支持的 driver 忽略）；claudecode `sessionFlags` 在非空时拼 `--mcp-config <path>`。`butler.Engine` 仅当解析到的会话 `Role==butler` 时注入 `Deps.ButlerMCPConfig`，worker/普通会话不带。`butlermcp.WriteConfig` 生成指向 `mcp-server` 子命令（#79）的 stdio MCP 配置文件，启动时写到 `~/.bbclaw-adapter/butler-mcp.json`。
  - **WarmPool 预热管家**：`claudecode.WarmPool` 从单 `warmCwd` 扩成多 `warmCwds`（项目 cwd + 管家 workspace），每 cwd 独立维持 `size` 个预热条目，`Acquire(cwd)` 严格按 cwd 命中——管家每轮命中预热，避免 4-7s 冷启动。
- **管家 MCP 派发 server — worker runner + `mcp-server` 子命令 (ADR-021 v1)**：补齐「管家 MCP 派发」最后一公里。
  - **`ClaudeWorkerRunner`**（`adapter/internal/butlermcp/runner_claude.go`）：落地 `WorkerRunner` 接口，复用 claudecode driver 在目标 cwd 起 worker（`--permission-mode acceptEdits`），消费 stream-json 累积 assistant 文本到 `EvTurnEnd`，`EvError` 透传为错误，输出超长时按头尾保留、中间省略裁剪（默认 8KB，避免把超长 transcript 回灌管家）。
  - **`bbclaw-adapter mcp-server` 子命令**（`adapter/internal/cmd/mcpserver.go`）：从 env 读 `BBCLAW_CWD_POOL`（allowlist）与 `ANTHROPIC_BASE_URL`/`ANTHROPIC_AUTH_TOKEN`，装配 `ClaudeWorkerRunner` + `butlermcp.New`，在 stdio 上 `Serve`；**stdout 仅 JSON-RPC，日志全部走 stderr**（`obs.NewLoggerTo`）。新增 `config.LoadButlerEnv` 仅加载管家所需字段，跳过 ASR/TTS 校验。
  - **e2e 冒烟**（`adapter/scripts/butler-mcp-smoke.sh` + `butler-mcp-config.example.json`）：协议层冒烟无需真实 claude；`BBCLAW_BUTLER_LIVE=1` 时跑 `claude -p --mcp-config` 真链路（claude 不在 PATH 时自动跳过，CI 不依赖）。
- **逻辑会话 Role 字段 + worker 不进设备菜单 (ADR-021, #82)**：为"设备 ↔ 管家(butler) ↔ N 个 worker"会话分层打底。`logicalsession.LogicalSession` 新增 `Role` 字段（`butler` / `worker` / 空=向后兼容的普通会话）及 `RoleButler`/`RoleWorker`/`RoleNone` 常量；`Manager.CreateWithRole` 支持指定角色（`Create` 保持原签名、默认空 role，现有 5 个调用点零改动）。新增 `Manager.ListDeviceFacing`（在 limit 之前先剔除 worker），4 处设备朝向入口改用它：`httpapi` 的 sessions 菜单与 `handleAgentSessionsLogical`、`homeadapter/agent_proxy` 的 cloud relay 列表与菜单两处镜像逻辑——确保 cloud 模式下 worker 也不泄漏给设备。底层 `List` 仍返回全量供 butler/dispatch 使用。旧无 Role 记录反序列化为空 role 仍按普通会话列出；角色的实际写入由 #80(butler)/#79(worker) 消费。

## [0.4.4] - 2026-05-29

### Fixed
- **电池电量显示不准 (P0)**：第一版线性映射（3300mV→0%，4200mV→100%）不符合锂电池放电曲线，导致满电掉电飞快、中段卡住、低电量突然归零。`bb_power.c` 改为 OCV–SoC 放电曲线查表 + 线性插值，并新增三项滤波：(1) 跨周期 EMA 电压低通（`BBCLAW_POWER_EMA_ALPHA_PCT`，默认 25%）吸收 PTT/功放/WiFi 负载瞬态；(2) 百分比迟滞（`BBCLAW_POWER_HYSTERESIS_PCT`，默认 2%）消除 ±1% 抖动；(3) ADC dummy read 规避高阻分压首读偏低。曲线表与方案见 `firmware/docs/feat/power-management-foundation.md`。充电检测仍为 TODO（需 VBUS 脚）。
- **CHAT 最外层长按 OK 进不了菜单**：新 PCB rev 把 OK 移到编码器按压（IO1），`bb_nav_input` 把长按 OK 改发 `BB_NAV_EVENT_BACK` 替代旧的 `OK_LONG`，但 `bb_radio_app` CHAT 状态里"进 SETTINGS"那条路径还挂在 `OK_LONG` 上，导致长按落到空 BACK case 上没反应。把进 SETTINGS 行为合并到 BACK 处理里——busy 时维持取消 in-flight turn 的旧语义，空闲时进入 SETTINGS 浮层。

### Added
- **TTS 阅读模式 + Chat 本地 tail 缓存 (ADR-017)**：解决两个 UX 痛点。
  - **阅读模式**：TTS 播报中按 UP 翻看历史不再被下一句 chunk 拉回底部 — chat transcript 加了 `follow_tail` 锁存，UP 即进入阅读模式，DOWN 滚回底部自动恢复 follow，期间底栏显示 "● 阅读中 (DOWN 到底回到实时)" 提示。
  - **Chat tail 缓存**：每个 driver 在 NVS 里维护一个 1.5KB 的最近消息环（key `cc/<驱动短码>`），睡眠/唤醒回到 chat 时先用本地 cache 渲染最近几条消息，再 fire adapter fetch；adapter 不在线也能看到刚才的对话。Fetch 成功后清缓存重写以保持远端为 SoT。
- **TTS 文本清洗 `tts.Sanitize`**：`/v1/tts/synthesize` 在送入 provider 之前先剥掉 markdown 加粗/斜体/反引号、代码块围栏、ATX 标题、列表/引用前缀、`[文本](链接)`、HTML 标签、零宽与控制字符，并把多行/多空白塌缩成单空格。`say` 之前会把 `**Sonnet 4.5**` 念成"星号星号 Sonnet 4.5"、反引号包路径段也会让发音断裂，清洗后这些都按正常文本播报，保证一段完整内容能跑完。日志里同时带 `text_chars`（清洗后）与 `raw_chars`（原始）便于排查。
- **设备端 Driver / Model 选择** (ADR-016)：Settings 屏改为二级菜单（主屏 Driver/Model/TTS/Back，OK 进同名 picker 子屏），用户可在 BBClaw 上直接切换 active driver（claude-code / opencode / aider / ollama / openclaw）和当前 driver 的 model（Sonnet / Opus / Haiku / GPT-5 / Ollama tags 等），无需 SSH 进 adapter 改 env。Adapter 持久化 `~/.bbclaw-adapter/driver_state.json`，多设备共享。
  - Adapter HTTP：`GET /v1/agent/drivers` 扩展返回 `active_driver` + 每 driver 的 `models[]` 与 `active_model`；新增 `PUT /v1/agent/active_driver` 与 `PUT /v1/agent/drivers/{name}/active_model`
  - Cloud envelope：扩展 `agent.drivers` payload；新增 `agent.active_driver.set` / `agent.active_model.set` kind（cloud_saas 模式下需 cloud 侧配套补 HTTP proxy 才能在远端生效；local_home 模式立即可用）
  - Driver 接口：可选 `ModelLister` 接口；claude-code / opencode / aider 提供静态模型列表，ollama 调 `/api/tags` 60s 缓存；`agent.StartOpts.Model` 字段把选定 model 注入 session 启动参数
  - Firmware：新增 NVS key `drv/active`、`bb_agent_set_active_driver` / `bb_agent_set_active_model`；chat 屏 driver fallback 改读 NVS
  - **UI 修订（2026-05-17）**：Settings 从最初的"扁平 5 行 + LEFT/RIGHT 循环值"改为"二级菜单 + 旋钮 + OK + BACK"，因为硬件实际无 LEFT/RIGHT 物理键（首版方案误用了代码里的残留事件）；Session Picker 去除原 Driver 切换行，driver 选择统一收归 Settings，两个 UI 重复入口消除。

## [0.4.3] - 2026-05-16

### Fixed
- **Adapter home_site_id 在 macOS 上每次重启变化**: 原来用 hostname + user + machine-id 派生 UUID v5，macOS 没有 `/etc/machine-id` 且 hostname 随网络切换变化，导致每次启动生成不同 ID，cloud 端认为是新设备并重新要求 claim。改为首次启动时生成随机 UUID v4 并持久化到 `~/.bbclaw-adapter/identity.json`，后续从文件读取，彻底消除漂移。

## [0.4.1] - 2026-04-27

First release using the unified `release.yml` workflow — firmware OTA bin
and adapter binaries (5 platforms) ship together from a single `v*` tag.

### Added
- **Buddy-anim theme** (Phase 4.6.x): LVGL-driven 9-state animations on top of the
  existing ASCII faces — opacity pulse for SLEEP, y-bob for IDLE/LISTENING,
  dot cycle for BUSY, x-sway for SPEAKING, transform-scale heartbeat for HEART,
  color lerp for ATTENTION, y-bounce + festive color for CELEBRATE,
  fast x-shake for DIZZY. Selectable via NVS like the existing themes.
- **Aider driver** (Phase 4.10): adapter-side driver wrapping the `aider` CLI in
  non-interactive mode (`--message ... --yes-always --no-pretty --no-stream`),
  with per-session `--chat-history-file` for multi-turn continuity. Auto-enabled
  when `aider` is on PATH; overridable via `AGENT_AIDER_FORCE`.
- **Adapter source open-sourced** (ADR-011): the Go adapter moved from
  `bbclaw-reference/adapter` to this repo at `adapter/`, preserving git
  history (subtree split). Module path `github.com/daboluocc/bbclaw/adapter`
  is unchanged. Cloud backend and web portal stay closed.

### Fixed
- **PTT → LISTENING state**: pressing PTT now reliably transitions the buddy face
  to LISTENING. Previous code gated the state change behind WiFi / TTS guards
  that could silently skip it (only the haptic fired); recording still started
  via the arm path, leaving the user without visual feedback.
- **Double-OK kills settings**: rapid OK presses no longer cause the settings
  overlay to flash open then immediately close. The nav-event drain skips
  buffered OK/BACK events for one tick after `settings_overlay_enter()` succeeds.
- **Settings overlay panic-rebooted from chat**: `bb_ui_settings_show()` did
  synchronous NVS reads from the caller's task. The chat→OK→settings path
  runs on `stream_task`, which is allocated on PSRAM stack; NVS internally
  disables SPI flash cache and asserts the stack is in internal RAM, so
  the device hard-rebooted the moment the user opened settings from chat.
  Fix: new `bb_ui_settings_preload_nvs()` is called once from
  `bb_radio_app_start()` (internal-RAM stack), and `load_*_from_nvs()` are
  now idempotent — later calls from any task become pure memory reads.
- **Capture ringbuf overflow on cold-start**: the first HTTP chunk of a PTT
  utterance takes 500–700 ms (TCP connect + first write); the 8-chunk
  (~480 ms) capture ringbuf was just under that ceiling and dropped audio
  at the start of every utterance. Bumped to 32 chunks (~1.9 s in PSRAM).
- **Adapter Makefile ldflags** silently failed to inject build tag/time —
  the `-X` package path was the wrong module name. Pre-existing bug in the
  closed repo, fixed during the migration.

## [0.4.0] - 2026-04-27

### Added
- **Multi-Driver Agent Bus**: support CLI-based agent drivers — Claude Code, OpenCode, Ollama — alongside original OpenClaw
- **LEFT/RIGHT quick driver switching**: carousel-style driver cycling from chat overlay with session auto-reset
- **Agent Chat as home screen**: boot directly into Agent Chat overlay; 90s idle timeout exits to standby
- **Cancel in-flight turn**: OK/BACK during agent thinking cancels the turn (discard events, kill TTS, show IDLE)
- **Chat transcript scrolling**: UP/DOWN scrolls the agent chat transcript (2 lines/press) within the overlay
- **PTT voice bridge**: PTT-to-Agent-Bus pipeline — record → ASR → agent → streaming TTS reply
- **Streaming TTS**: sentence-level TTS with cancel-and-replace on new user turn
- **Buddy-ASCII theme**: seven-state animated character alongside chat transcript
- **Flipper 6-button navigation**: UP/DOWN/LEFT/RIGHT/OK/BACK mapped to 5-way nav module
- **Standalone Settings overlay**: OK opens Settings from chat; driver/theme/TTS toggle persisted to NVS
- **Async driver fetch**: pre-warm driver list on chat entry; non-blocking HTTP cache
- **Device identity in Agent Bus URLs**: deviceId query param for cloud proxy routing

### Fixed
- **PSRAM-stack NVS crash**: NVS reads spawned on internal-RAM task to avoid `cache_utils` assert when `stream_task` (PSRAM stack) triggers SPI flash
- **Driver switch HTTP 400**: clear `session_id` on driver cycle so adapter creates fresh session
- **NVS write deferred**: `cycle_driver` NVS persist offloaded to background task (avoids SPI-flash cache collision)
- **TTS task stack**: bumped 4K → 8K for Phase 4.5.2 streaming pipeline
- **PTT → BUSY transition**: immediate BUSY state on PTT release before cloud-wait
- **Block driver cycling while busy**: LEFT/RIGHT blocked during agent turn in flight

## [0.3.5] - 2026-04-16

### Added
- **GitHub Actions CI**：推送 tag 时自动构建并上传固件到 OTA 服务器

## [0.3.4] - 2026-04-16

### Added
- **OTA 在线升级**：云端连接成功后自动检查并下载固件更新
- **双分区 OTA**：支持 ota_0/ota_1 交替升级，2.5MB 分区空间
- **OTA 状态机**：`bb_ota.c/h` 实现检查/下载/校验/烧写完整流程
- **升级庆祝**：更新成功后首次启动显示"更新成功!"画面
- **固件版本上报**：设备信息包含固件版本，云端可查看

### Fixed
- 分区表：添加缺失 otadata 分区，修正 ota_0 起始地址
- JSON 解析：`hasUpdate` 字段偏移修复 (11→12)
- Makefile flash 地址：0x110000 → 0x120000

## [0.3.3] - 2026-04-15

### Added
- 固件状态机重构：新增 `bb_status.h` 集中定义所有 status 字符串常量
- 状态机文档：`design/STATE_MACHINE.md` 完整描述 AP/锁屏/正常/待机/问答模式
- 状态转换追踪：LOCKED ↔ UNLOCKED 切换时输出 `STATE_TRANSITION` 日志

### Changed
- 重构 `bb_radio_app.c`、`bb_lvgl_display.c`、`bb_display_bitmap.c` 使用 BB_STATUS_* 常量

## [0.3.2] - 2026-04-13

### Fixed
- Adapter WS 心跳：每 25 秒发送 ping，彻底解决 35 秒断连重连循环
- Adapter 并发写安全：所有 `conn.WriteJSON` 统一走带 `sync.Mutex` 的 `writeConn`，防止并发写崩溃

### Changed (Web)
- Home Adapter 详情页展示 Adapter 版本号、运行平台、构建时间

## [0.3.1] - 2026-04-13

### Added
- Adapter 连接云端后自动上报版本号、平台、构建时间（Portal Home Adapter 页可查看）

## [0.3.0] - 2026-04-13

### Added
- Web 对话（Web Chat）：登录后可在 Portal 直接通过浏览器与 OpenClaw 对话，无需持有 BBClaw 硬件
- 流式输出：回复逐字流式显示（SSE），支持停止按钮
- 对话历史：每次会话结果持久化存储，切换设备时自动加载最近 50 条
- Adapter 新增 `chat.text` 请求类型，文字直接转发至 OpenClaw，跳过 ASR 步骤

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
