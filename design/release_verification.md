# v0.4.0 真机验证清单

烧录完成后跑这一份清单。自动化部分见 `firmware/scripts/verify_release.py`，
手动部分覆盖导航键矩阵、四 driver PTT 链路、九态视觉、空闲退出与已知陷阱。

> 读这份文档前请确认 CHANGELOG/release notes 已对齐 v0.4.0 范围。
> 出现 ✗ 应当先排查再发版本，不要带病发布。

---

## 1. 烧录前准备

- [ ] USB 接好；`/dev/cu.usbmodem*` 在 macOS 上能 `ls` 看到一个口
- [ ] 5-向 nav 模块接线：
  - COM → GND
  - UP → GPIO **6**
  - DOWN → GPIO **8**
  - LEFT → GPIO **38**
  - RIGHT → GPIO **39**
  - MID (OK) → GPIO **1**
  - RST (BACK) → GPIO **47**
- [ ] PTT 按键：一脚 → GPIO **7**，另一脚 → GND
- [ ] adapter 起在 `192.168.1.2:18080`（或与设备 NVS 一致的地址）
- [ ] adapter 侧 4 个 driver env 都配齐：
  - `ANTHROPIC_API_KEY` (claude-code)
  - `OPENCLAW_*` (openclaw)
  - `OLLAMA_HOST` (ollama)
  - `OPENCODE_*` (opencode)
- [ ] 设备能连的 2.4G WiFi（如 `xiaomi-5-505`）SSID/密码已在 NVS 里
- [ ] 烧录命令已跑：`cd firmware && make flash`
- [ ] **不要**手动 `make monitor`（项目规则禁止），后续读日志走 `make monitor-last`

---

## 2. Boot 验证（自动化）

```bash
cd firmware
./scripts/verify_release.py
# 或显式指定端口/超时
./scripts/verify_release.py --port /dev/cu.usbmodem2112401 --timeout 45
```

期待输出：

```
  [OK]   bootloader-second-stage          <- '2nd stage bootloader'
  [OK]   app-init                         <- 'app_init: Project name:     bbclaw_firmware'
  [OK]   theme-text-only-registered       <- "bb_agent_theme: register: 'text-only'"
  [OK]   theme-buddy-registered           <- "bb_agent_theme: register: 'buddy-ascii'"
  [OK]   active-theme-resolved            <- 'bb_radio_app: boot active theme:'
  [OK]   nav-init-flipper6                <- 'bb_nav_input: nav init mode=flipper6'
  [OK]   wifi-connected                   <- 'bb_wifi: wifi connected'
  [OK]   transport-health                 <- 'bb_radio_app: transport health ok status=200'

release verification PASSED in N.Ns
```

任何 `[MISS]` 都阻塞发布。脚本会打印 capture tail 帮助 debug。

- [ ] 8/8 全 OK
- [ ] elapsed < 30s

---

## 3. 导航键测试矩阵 (4 context × 6 keys = 24 cells)

每一格按一下，确认行为符合下表。无变化或反向 = ✗。

| Context        | UP                              | DOWN                            | LEFT                                     | RIGHT                                    | OK                                  | BACK                              |
| -------------- | ------------------------------- | ------------------------------- | ---------------------------------------- | ---------------------------------------- | ----------------------------------- | --------------------------------- |
| 待机主屏       | 滚 chat 历史，上一条 turn       | 同，下一条 turn                 | （保留，无操作）                         | （保留，无操作）                         | 进 Settings                         | 进 Agent Chat overlay             |
| Agent Chat     | scroll transcript -2 行         | scroll transcript +2 行         | cycle driver -1（busy 时阻塞 + 提示）    | cycle driver +1（busy 时阻塞 + 提示）    | 发 picker；busy 时 cancel 当前 turn | 进 Settings；busy 时 cancel       |
| Settings       | 行光标 -1                       | 行光标 +1                       | 当前行预览值 -1                          | 当前行预览值 +1                          | 提交当前行 + 推进                   | 退出 Settings 回上一层            |
| Locked         | （锁屏 voice unlock 优先；nav 暂屏蔽） | 同左                            | 同左                                     | 同左                                     | 同左                                | 同左（仅 voice unlock 解锁）      |

- [ ] 6×4 = 24 格全部符合预期
- [ ] LEFT/RIGHT 在 Agent Chat busy 期间确实被阻塞（不会乱切 driver）

---

## 4. PTT 全链路 — 4 driver 各一遍

对每个 driver in `(claude-code, openclaw, ollama, opencode)`：

1. 进 Agent Chat（待机主屏 → BACK）
2. 再 BACK 一次进 Settings（或 OK 也可，看 v0.4.0 实际入口）
3. 旋到 `Agent` 行
4. LEFT/RIGHT 切到目标 driver
5. OK 提交，确认 NVS 落盘日志
6. 退出 Settings 回 chat
7. 按住 PTT 说一句"你好"，松开
8. 观察：
   - [ ] buddy face 序列：`IDLE → LISTENING → BUSY → SPEAKING → IDLE/HEART`
   - [ ] 屏幕 transcript 出现回复文字
   - [ ] 听到 TTS 播放（macos_say 或本机 TTS）
   - [ ] 日志里能看到 `bb_agent: send_message ... driver=<DRIVER>`，driver 字段对得上

| Driver       | NVS 切换 OK | LISTENING | BUSY | SPEAKING | transcript | TTS |
| ------------ | ----------- | --------- | ---- | -------- | ---------- | --- |
| claude-code  |             |           |      |          |            |     |
| openclaw     |             |           |      |          |            |     |
| ollama       |             |           |      |          |            |     |
| opencode     |             |           |      |          |            |     |

---

## 5. 状态视觉清单 — 9 states

每个状态期待的 face / mood 字符串 / 触发方式。看屏 + 抓日志比对。

| state      | face       | mood            | trigger                                   |
| ---------- | ---------- | --------------- | ----------------------------------------- |
| SLEEP      | `(-_-)`    | `zzz...`        | 开机即态 / 长时间无交互后回此态           |
| IDLE       | `(^_^)`    | `ready`         | turn 完成（普通耗时）后回此态             |
| BUSY       | `(o_o)`    | `thinking...`   | agent 在算（ASR / agent_task / EvText）   |
| ATTENTION  | `(O_O)?`   | `your turn`     | （v0.4.0 不会自然触发，留给 tool_call）   |
| CELEBRATE  | `\(^o^)/`  | `yay!`          | （v0.4.0 不会自然触发，留给 token 阈值）  |
| DIZZY      | `(X_X)`    | `oops...`       | EvError（强制断网/打错 driver token 触发）|
| HEART      | `(^_^)`    | `<3`            | turn < 5s 完成（"快"奖励态）              |
| LISTENING  | `(o.o)"`   | `listening...`  | PTT 按住录音中                            |
| SPEAKING   | `(^o^)~`   | `speaking...`   | TTS 播放中                                |

- [ ] 7 个能自然触发的态都见过一次（`SLEEP / IDLE / BUSY / DIZZY / HEART / LISTENING / SPEAKING`）
- [ ] `ATTENTION` 和 `CELEBRATE` 在 v0.4.0 不可达 — 列出供以后版本使用，不阻塞发版

---

## 6. 空闲超时

- [ ] 进 chat overlay 后保持 90 秒不动任何键、不按 PTT
- [ ] 屏幕回到主屏（chat overlay 消失，回 standby buddy face）
- [ ] `make monitor-last | grep -i 'idle.*exiting chat'` 能看到 `idle ... ms: exiting chat to standby`
- [ ] 按任意 nav 键 → 重新进 chat overlay（不丢历史 transcript）

---

## 7. 已知陷阱 sanity 检查

- [ ] **NVS deferred 不 panic**：在 Agent Chat 里反复快速 LEFT/RIGHT 切 driver 10+ 次，
      不应触发 panic / brownout。修复见 commit `f33e232`（NVS 写入挪到 deferred path）。
- [ ] **TTS 长 reply 不爆栈**：让 agent 输出 ≥ 500 字一段长文，TTS 完整播完，
      不应 stack overflow。修复见 commit `0fe8be9`（TTS task 栈拉到 8K）。
- [ ] **BACK 键不卡 boot**：插 USB 时**不要**按住 BACK（GPIO 47），
      理论上已避开 BOOT strap，但发版前还是手动确认一次冷启可正常进 app。
- [ ] **driver 切换在 SPEAKING 期不应抢占**：SPEAKING 中按 LEFT/RIGHT，
      应该被 busy 阻塞（提示音或屏幕 hint），不会半路切 driver 把当前 reply 截断。

---

## 8. 发版决策

- [ ] §2 自动化全 OK
- [ ] §3 24 格矩阵无 ✗
- [ ] §4 4 driver 全链路 OK
- [ ] §5 7/9 状态见过
- [ ] §6 空闲退出 OK
- [ ] §7 4 个陷阱全 OK

全勾 → 可以打 tag 发 release（按 CLAUDE.md 的 Release & Tag Policy 决定要不要建 GitHub Release）。
有任何 ✗ → 回到对应模块修，不要"先发再说"。
