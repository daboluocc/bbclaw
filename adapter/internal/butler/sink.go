package butler

import "github.com/daboluocc/bbclaw/adapter/internal/agent"

// EventSink 抽象「把一帧 agent 事件/会话帧发给设备」。差异 #1 的核心。
// butler 只调用语义化方法,序列化由各 transport 自己实现。
//
// 关键:butler 内部用 EmitEvent 替代 LOCAL writeAgentEvent / CLOUD agentEventToFrame,
// 用 EmitSession 替代两边手写的 session map,用 EmitError 替代 UNKNOWN_LOGICAL_SESSION
// 等流式 error 帧。EvSessionInit 一律不外发(两边一致:LOCAL writeAgentEvent return true
// 跳过;CLOUD agentEventToFrame return nil 跳过),由 butler 在调 EmitEvent 前过滤。
//
// 返回 ok=false 表示客户端断开,butler 立即 return(复刻 LOCAL writeAgentEvent==false
// 的 return)。CLOUD 实现为 best-effort,从不因写失败而返回 false。
type EventSink interface {
	// EmitSession 发会话帧(type=session, sessionId=visibleID, isNew, driver, seq=0)。
	EmitSession(visibleID string, isNew bool, driver string) bool
	// EmitEvent 发一个 agent 事件(text/tokens/tool_call/error/turn_end)。
	// butler 已过滤掉 EvSessionInit;suppress 逻辑也在 butler 内,不会传抑制掉的帧。
	EmitEvent(ev agent.Event) bool
	// EmitError 发解析期/运行期的流式 error 帧(如 UNKNOWN_LOGICAL_SESSION、
	// AGENT_START_FAILED)。code 进 "error" 字段,text 进 "text"/"detail"。
	// detailField=true 时进 "detail"(AGENT_START_FAILED 用),否则进 "text"。
	EmitError(code, text string, detailField bool) bool
}
