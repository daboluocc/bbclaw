package butler

import (
	"fmt"
	"sync"

	"github.com/daboluocc/bbclaw/adapter/internal/agent"
)

// InflightRegistry 跟踪每台设备当前正在跑的 turn,供 barge-in 打断(ADR-028 §2.5.1)。
//
// 一台设备同一时刻最多一个 in-flight turn(设备侧串行),key 是 deviceID。
// 设备在某些路径上不带 deviceId,所以 Cancel 在精确 key 未命中且注册表里
// 恰好只有一个 turn 时回退到那一个(单设备部署是绝对主流)。
//
// ADR-028 §2.5.1 修订(撤回语义,2026-06):打断 = 撤回上一回合,当没发生过。
// Cancel 只杀掉回合子进程(保留 session/resumeID),**不再**记录"打断备注"
// 注入下一回合的 prompt——下一轮有新语音时直接 --resume 干净续接,模型不被
// 告知"你被打断了"。(Claude Code 增量持久化 JSONL 仍会留下被截断的半截
// assistant 输出,这是底层限制;撤回仅指不主动注入备注。)
type InflightRegistry struct {
	mu    sync.Mutex
	seq   uint64
	turns map[string]*inflightTurn
}

type inflightTurn struct {
	tok uint64
	drv agent.Driver
	sid agent.SessionID
}

// NewInflightRegistry 构造空注册表。
func NewInflightRegistry() *InflightRegistry {
	return &InflightRegistry{
		turns: make(map[string]*inflightTurn),
	}
}

// Begin 登记一个开始执行的 turn,返回用于 End 的 token。同一 device 的旧登记
// 被覆盖(retry 场景)。
func (r *InflightRegistry) Begin(deviceID string, drv agent.Driver, sid agent.SessionID) uint64 {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.seq++
	r.turns[deviceID] = &inflightTurn{tok: r.seq, drv: drv, sid: sid}
	return r.seq
}

// End 注销 turn。仅当 token 仍是该 device 的当前登记时生效(陈旧 token no-op)。
func (r *InflightRegistry) End(deviceID string, tok uint64) {
	r.mu.Lock()
	defer r.mu.Unlock()
	if t, ok := r.turns[deviceID]; ok && t.tok == tok {
		delete(r.turns, deviceID)
	}
}

// Cancel 打断 deviceID 当前的 in-flight turn:调用驱动的 Interrupter 能力杀掉
// 回合子进程(连带其中的工具执行),保留 session/resumeID 供下一回合 --resume。
//
// 返回 found=false 表示没有 in-flight turn(本地播放可能仍在跑,设备自行停止);
// err 非 nil 表示找到了 turn 但驱动不支持 Interrupt 或调用失败。
func (r *InflightRegistry) Cancel(deviceID string) (found bool, err error) {
	r.mu.Lock()
	t, ok := r.turns[deviceID]
	if !ok && len(r.turns) == 1 {
		for _, v := range r.turns {
			t, ok = v, true
		}
	}
	r.mu.Unlock()
	if !ok {
		return false, nil
	}
	ir, can := t.drv.(agent.Interrupter)
	if !can {
		return true, fmt.Errorf("driver %s does not support interrupt", t.drv.Name())
	}
	return true, ir.Interrupt(t.sid)
}
