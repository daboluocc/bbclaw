// Package butler 下沉 LOCAL(httpapi)与 CLOUD(homeadapter)两份 turn 编排骨架。
// 仅依赖 internal/agent、internal/agent/logicalsession、标准库。绝不反向依赖
// httpapi/homeadapter。本文件承载策略开关、hook、结构化错误与轻量接口。
package butler

import "time"

// Policy 是各 caller 注入的纯行为开关。butler 据此在共享骨架里走 LOCAL / CLOUD
// 各自的历史分支,而不引入任何行为变化。
type Policy struct {
	// ReuseWindow 控制空 sessionId 时的会话复用窗口(差异 #3)。
	// 0 = 关(CLOUD 恒 0,空 id 永远 Create)。
	// LOCAL 传 cfg.SessionReuseWindow;>0 且 Sessions!=nil 时,对空
	// requestedSession 先 FindRecent(deviceID, driverName, ReuseWindow)。
	ReuseWindow time.Duration

	// AllowBareCLIID 控制裸 CLI id(非空、无 ls- 前缀)的处理(差异 #4)。
	// LOCAL=false:返回 INVALID_SESSION_ID(PreStream 400)。
	// CLOUD=true :走 legacy registry 查找(命中则 pin,带 mismatch/unregistered
	// 校验;未命中则当作裸 cli id 由 attempt0 用作 ResumeID)。
	AllowBareCLIID bool

	// AutoTitle 控制自动标题(差异 #6)。
	// LOCAL=true :turnEnded 且 logical 无 title 时,SetTitle(首条消息前 20 runes)。
	// CLOUD=false:跳过。
	AutoTitle bool

	// EmitTurnEndFrame 控制 EvTurnEnd 是否作为一帧经 Sink.EmitEvent 外发(矩阵外补充)。
	// LOCAL=true :turn_end 作为 NDJSON 一帧外发后 break。
	// CLOUD=false:仅置 turnEnded=true 然后 break,不外发独立 turn_end 帧
	// (复刻原 `break loop` 行为)。
	EmitTurnEndFrame bool

	// EmitStartFailedFrame 控制 drv.Start 失败时是否经 Sink.EmitError 发流式 detail
	// 帧(矩阵外补充)。LOCAL=true(发 detail 帧);CLOUD=false(仅靠返回的
	// CodedError 让 caller 转 Go-error)。
	EmitStartFailedFrame bool

	// MaxAttempts 恒为 2(常量化,留 knob 防御性)。<=0 时按 2 处理。
	MaxAttempts int
}

// maxAttempts 返回受控的尝试上限。
func (p Policy) maxAttempts() int {
	if p.MaxAttempts <= 0 {
		return 2
	}
	return p.MaxAttempts
}

// Notification 是收尾通知的纯数据(差异 #7)。butler 不做任何截断,
// 截断语义保留在各 caller 的 hook 里(LOCAL pushNotification / CLOUD envelope)。
type Notification struct {
	SessionID string // 设备可见 id(visibleID)
	Driver    string
	Type      string // "turn_end" | "error"
	Preview   string // = lastText,未截断
}

// Hooks 是各 caller 注入的收尾/状态回调(差异 #5 / #7)。任意字段可为 nil,
// butler 调用前判 nil。
type Hooks struct {
	// OnStateChange 在 EvError(非重试分支)调 (visibleID,"error",ev.Text),
	// 在收尾(!ChannelClosed)调 (visibleID,"completed",lastText)。
	// LOCAL 传 broadcastSessionStateChange 包裹;CLOUD 传 nil。
	OnStateChange func(visibleID, state, preview string)

	// OnTurnComplete 仅在 turnEnded 时调用一次(差异 #7)。
	// LOCAL 传 pushNotification 包裹;CLOUD 传 session.notification envelope 包裹。
	OnTurnComplete func(n Notification)

	// OnFinalReply 在 RunTurn 末尾调用一次(差异 #7 CLOUD-only)。
	// CLOUD 据 Result 构造 agent.reply envelope;LOCAL 传 nil。
	OnFinalReply func(r *Result)
}

// MetricsSink 抽象指标计数。butler 在生命周期点调用语义化方法,由各 caller 映射到
// 自己【精确的历史指标名】与【判定条件】。这是有意为之:LOCAL 与 CLOUD 历史上的
// 指标命名(CLOUD 的 start/ok/error 带 _message_ 中缀,retry/resume_skipped 不带)
// 与收尾 ok/error 的判定条件(CLOUD 额外要求 turnEnded)就不一致,无法用统一的
// 字符串前缀拼接复刻——否则会改名或双计数。
type MetricsSink interface {
	// TurnStart 在解析完成、进入 attempt 循环前调用一次。
	TurnStart()
	// ResumeSkippedMissing 在主动 resume 校验判定磁盘无会话、跳过 resume 时调用。
	ResumeSkippedMissing()
	// SessionNotFoundRetry 在每次 SESSION_NOT_FOUND 透明重试时调用。
	SessionNotFoundRetry()
	// TurnDone 在 turn 收尾时调用一次,传入原始信号由 caller 自行判定 ok/error 分支
	// 与具体指标名(LOCAL:errorCount>0&&textCount==0 → error_only,否则 ok;
	// CLOUD:turnEnded&&(errorCount==0||textCount>0) → ok,否则 error)。
	TurnDone(turnEnded bool, textCount, errorCount int)
}

// Logger 抽象 butler 内部的结构化日志。
type Logger interface {
	Infof(format string, args ...any)
	Warnf(format string, args ...any)
	Errorf(format string, args ...any)
}

// ─────────────────────────── 结构化错误 ───────────────────────────

// CodedError 是解析期(pre-stream)或运行期结构化错误。Code 是稳定机器码;
// PreStream=true 表示「LOCAL 必须在翻成流式之前回 4xx JSON」的那一类;
// false 表示运行期/流式错误。HTTPStatus 仅供 LOCAL 选择状态码(CLOUD 忽略)。
// 差异 #2 的载体。
type CodedError struct {
	Code string // "UNKNOWN_DRIVER" / "SESSION_DRIVER_MISMATCH" / "INVALID_SESSION_ID" /
	// "UNKNOWN_LOGICAL_SESSION" / "SESSION_UNREGISTERED_DRIVER" / "CREATE_SESSION_FAILED" /
	// "AGENT_START_FAILED" / ...
	Detail     string
	PreStream  bool  // true:校验类(LOCAL 回 JSON 4xx);false:流式 error 帧
	HTTPStatus int   // 400 / 500 / 501;CLOUD 不使用
	Err        error // 底层 error(CREATE_SESSION_FAILED / AGENT_START_FAILED 等),可为 nil
}

func (e *CodedError) Error() string { return e.Code }
func (e *CodedError) Unwrap() error { return e.Err }

// 常用构造器(按各 caller 现有码值精确复刻)。caller 在进入 RunTurn 前自行处理
// 这两类,butler 不触发它们;列出以示已覆盖。
var (
	ErrEmptyText     = &CodedError{Code: "EMPTY_TEXT", PreStream: true, HTTPStatus: 400}
	ErrNotConfigured = &CodedError{Code: "AGENT_NOT_CONFIGURED", PreStream: true, HTTPStatus: 501}
)

// truncateRunes 返回 s 的前 n 个 rune。不足 n 个时原样返回。
func truncateRunes(s string, n int) string {
	runes := []rune(s)
	if len(runes) <= n {
		return s
	}
	return string(runes[:n])
}
