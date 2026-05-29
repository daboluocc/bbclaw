package butler

import "github.com/daboluocc/bbclaw/adapter/internal/agent"

// SessionRegistry 抽象 live cli-session 注册表(差异 #8)。butler 只用这 5 个动作。
// LOCAL 的 *sessionEntry 含 state(有 setState);CLOUD 的 *agentProxySession 无 state
// (setState no-op)。差异 #11(sid!="" else 分支 setState running)通过 SetState 抽象,
// 各自实现保留行为。
type SessionRegistry interface {
	// Get 返回 (driverName, sid, ok)。butler 不需要 entry 的其他字段。
	Get(id string) (driverName string, sid agent.SessionID, ok bool)
	// Put 注册一条新 live 会话(刚 drv.Start 出来)。LOCAL 建 *sessionEntry{state:"running"};
	// CLOUD 建 *agentProxySession。
	Put(id string, driverName string, sid agent.SessionID)
	// Touch 更新 lastUsed(缺失安全)。
	Touch(id string)
	// Drop 删除一条(events 关闭 / SESSION_NOT_FOUND 重试 / 通道关闭)。
	Drop(id string)
	// SetState 设状态:"running"/"error"/"completed"。CLOUD 实现为 no-op。
	SetState(id string, state string)
}
