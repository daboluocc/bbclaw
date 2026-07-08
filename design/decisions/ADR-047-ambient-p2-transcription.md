# ADR-047: Ambient P2 解读管线（VAD → 批 ASR → digest → adapter 录音记录页）

- 状态：**Proposed（2026-07-09 起草；§7 交付形态暂取 A，用户不在场按推荐方案暂定，可回退）**
- 日期：2026-07-09
- 组件：cloud（主体）+ adapter（本仓，录音记录页）+ firmware（书签已就绪，无需改）
- 上游：ADR-044（Ambient Capture）§5.2 / §6 P2 / §7.6；P1a/P1b 已落地并真机验收

## 1. 目标

把已经躺在云端的 ambient 分段（`data/ambient/<deviceId>/<date>/<segId>.opus`，
P1b 已按字节落盘 + 30 天生命周期）**解读成文本**：静音精切 → 批价 ASR → 词级
transcript → 小时级摘要 → 每日 digest；PTT 书签作为高价值锚点入摘要。转写文本
呈现在**运行面 adapter 的 `/admin`「录音记录」页**（原始音频留云端短保留，文本
轻量长价值——ADR-044 §7.6 用户补充）。

出口条件（ADR-044 §6）：每天自动出 digest，成本 ≤¥7/设备/天；adapter 页可浏览转写。

## 2. 前置状态（已就绪，P2 不重做）

- 分段音频云端落盘：`handleDeviceWS` ambient.* 接收，段字节边收边落盘 + fsync + ack。
- **Ogg 完整性**：段首 OpusHead/OpusTags（BOS）正确，段尾已置 EOS（本仓 commit
  `d53ac79`，2026-07-09）——批 ASR 前的文件合规前提已满足。
- 书签：固件 `bb_ambient_sync` 发 `ambient.bookmark {sessionId, atMs}`，云端已收，
  P2 直接消费其 `atMs` 作为摘要锚点。
- 段索引：云端 `segments.jsonl`（append-only），P2 worker 以它为 job 队列。

## 3. 架构决策

### 3.1 解读管线（云端，复用 voice-clone-worker 模式）
落盘 job 状态 + 常驻轮询 goroutine（重启可恢复，仿 `runVoiceCloneWorker`）：

```
segments.jsonl(pending)
  → Silero VAD 精切（丢静音，ambient 关键语音仅 ~14% 时长 → 下游 ASR 量砍到 1/4~1/7）
  → 火山「录音文件识别」异步 API（批价，比流式 ASR 便宜 40-50%）
  → transcript.jsonl（词级时间戳，按 sessionId/date 组织）
  → 小时级摘要（LLM）
  → daily digest（LLM，聚合当天小时摘要 + 书签锚点）
```

- 单位：**按 session 而非按段**做 ASR 提交——火山异步识别对单文件有最短时长/
  排队开销，攒一个 session（或一小时窗口）的段拼接后一次提交更省。段拼接用
  Ogg 页级 concat（同 serial 需重编号；或各段独立提交后按 startedAtMs 归并
  transcript）。**默认按段独立提交 + 归并**，避免跨段重写 Ogg（实现简单、幂等好做）。
- job 幂等：每段/每 session 一条 job 记录（状态 queued/asr_running/asr_done/
  summarized/failed），worker 只推进未完成项；ASR 回调用火山 task_id 去重。
- 成本闸：VAD 后 ~3h 语音/天/设备 → 批转写 ≈¥3-7/天；LLM 摘要相对可忽略。
  worker 侧记 per-device 日累计时长/费用，超阈值告警（不自动停，先观测）。

### 3.2 转写交付到 adapter —— 复用 homeadapter WS（**本 ADR 的新契约**）

**澄清 ADR-044「不经 adapter/relay」**：那条约束是给**音频数据面**的（device→cloud
直连，不让每分钟一段的二进制流穿 home relay）。**转写文本是轻量、低频、已脱离音频
的产物**，走 adapter 与 cloud 之间**既有的 homeadapter WS 长连接**（本仓
`internal/homeadapter/adapter.go`，cloud_saas 下 adapter 常驻注册）最自然：无需给
adapter 发新的 cloud REST 凭证，无需新开轮询端点。

两种交付形态（交付语义决定 adapter 是否持久化本地副本）——**待用户拍板**：

- **A. 推送/同步（"下发"，推荐，贴合 §7.6「同步…展示」措辞）**：云端 digest/
  transcript 生成后，经 homeadapter WS 下发新 envelope kind；adapter **本地持久化**
  （`$BBCLAW_DATA_DIR/ambient/…`）成为文本的**长存副本**，云端音频/文本可按期清理。
  隐私模型 = 「你的转写落在你自己的运行面盒子上」。代价：需要投递 ack + 断线补投
  游标（adapter 侧记 last_delivered_cursor，重连拉增量）。
- **B. 拉取（adapter 作云端视图）**：adapter `/admin` 录音记录页按需向 cloud REST
  拉（adapter 用自身 binding 鉴权代理 `GET /v1/ambient/transcripts`）；云端是唯一
  真相，adapter 不持久化。实现最简、无投递重试，但文本寿命 = 云端寿命（与「文本
  长价值、音频短保留」略有张力，除非云端文本单独长存）。

> 默认方案 A，但把「文本是否以 adapter 本地为长存真相」作为唯一需确认的契约点
> （见 §7）。下面协议按 A 展开；选 B 则退化为一个 cloud REST + adapter proxy。

### 3.3 新 envelope kinds（cloud ↔ adapter，homeadapter WS）
```
cloud → adapter:
  ambient.digest      {deviceId, date, digestText, sessions:[{sessionId, summary,
                       startedAtMs, durationMs, bookmarks:[atMs...]}], cursor}
  ambient.transcript  {deviceId, sessionId, date, segments:[{segSeq, startedAtMs,
                       words:[{w, tMs}], text}], cursor}   # 可选：明细页才需要
adapter → cloud:
  ambient.sync.pull   {sinceCursor}          # 重连后拉增量
  ambient.deliver.ack {cursor}               # 收妥游标，云端可推进
```
- cursor = 单调递增交付序（云端每设备一条），断线补投的唯一依据（照 ambient 段
  ack 的 arm→finish→wait 纪律）。
- 协议同步表（CLAUDE.md）新增一行：`ambient.*` 转写 kind ↔ cloud homeadapter
  转发 + adapter 接收。

### 3.4 adapter 侧（本仓可先行的部分）
- 存储：`$BBCLAW_DATA_DIR/ambient/<deviceId>/<date>/digest.json` +
  `transcript-<sessionId>.json`（append/覆盖幂等，参考 adapter-runtime.log 的
  数据目录纪律 [[adapter-log-viewing]]）。
- HTTP：`GET /v1/admin/ambient/dates?deviceId=`、`.../digest?date=`、
  `.../transcript?sessionId=`（仿 `admin_logs.go` 的只读 admin 端点）。
- UI：`/admin` 新增「录音记录」tab（点阵设计语言，见 dot-matrix-ui skill）：
  日期列表 → 当日 digest（hero）→ 展开小时摘要 / session → 词级 transcript；
  书签锚点在时间轴上高亮。
- 删除自助：透传 ADR-044 §5.4 的 `DELETE /v1/ambient/data`（归属校验）。

## 4. 分片落地

| 片 | 内容 | 依赖 | 可在本仓? |
|----|------|------|----------|
| **S1 契约冻结** | 本 ADR + envelope kinds 定稿 + 协议同步表更新 | 用户拍 §7 | ✅ |
| **S2 cloud worker** | VAD 精切 + 火山异步 ASR + transcript.jsonl + job 状态机 | reference 仓 | ❌ 需克隆 cloud |
| **S3 cloud 摘要** | 小时摘要 + daily digest(LLM) + 书签入摘要 | S2 | ❌ cloud |
| **S4 交付** | cloud→adapter WS 下发 + cursor/ack + 断线补投 | S1,S3 | 两侧 |
| **S5 adapter 页** | 存储 + 只读 API + 录音记录 tab | S1 | ✅ 本仓 |

**当前阻塞**：S2/S3/S4 主体在 `bbclaw-reference`（cloud），本地**未克隆**
（gitignored `references/`）——需先 `git clone` 并 `git fetch` 对齐 origin/main
（[[cloud-reference-stale-clone]]：本地克隆常落后 + 混别会话未推 commit）。
本仓**能先做 S1（契约）+ S5（adapter 页，先对 mock 数据联调）**。

## 5. 成本与规模（沿用 ADR-044 §5.2）
- 存储：16kbps → 7.2MB/h，<180MB/天/设备；文本转写体量再小两个数量级。
- ASR：VAD 后 ~3h/天 批价 ≈¥3-7/天/设备（出口条件 ≤¥7）。
- V1 单 VPS 本地盘 + 生命周期；设备量上来再迁对象存储。

## 6. 隐私（PIPL，沿用 ADR-044 §5.4 + §8）
- 原始音频默认 30 天删、转写/摘要长存；账户级录音开关 + 知情同意。
- 方案 A 下文本长存真相在**用户自己的 adapter 盒子**，云端可更激进过期。
- **不做说话人分离/声纹**（敏感信息，需单独同意，V1 外）。
- 转写绝不进无鉴权页（`/debug/audio` 前车之鉴）；adapter 端点走 admin 鉴权。

## 7. 待确认（唯一契约决策）
**转写文本的长存真相在哪端?**
- **A 推送/同步到 adapter 本地**（推荐）：贴合「文本落你自己盒子」隐私模型 +
  §7.6 措辞；代价是 cursor/ack 断线补投一套。
- **B adapter 拉云端**：最简，但文本寿命绑云端。

其余（火山异步 ASR、Silero VAD、30 天音频、复用 voice-clone-worker、走 homeadapter
WS）均沿用 ADR-044 既定决策，不再开问。

## 8. 决策记录
1. **2026-07-09**：§7 交付形态**暂取 A（推送/同步到 adapter 本地为文本长存真相）**——
   用户不在场，按 ADR-044 §7.6 措辞与「文本落你自己盒子」隐私模型取推荐方案；
   属可回退的契约选择，落地实现前用户可改判 B。
2. 待补：cloud reference 仓克隆并对齐 origin/main 后确认 §3.1/§3.3 的落点符合现网结构。
