package butler

import (
	"fmt"
	"strings"
	"sync"
	"time"

	"github.com/daboluocc/bbclaw/adapter/internal/agent"
)

// InflightRegistry 跟踪每台设备当前正在跑的 turn,并保存 barge-in 取消后的
// 打断备注,供下一回合注入 prompt(ADR-028 §2.5.1)。
//
// 一台设备同一时刻最多一个 in-flight turn(设备侧串行),key 是 deviceID。
// 设备在某些路径上不带 deviceId,所以 Cancel 在精确 key 未命中且注册表里
// 恰好只有一个 turn 时回退到那一个(单设备部署是绝对主流)。
type InflightRegistry struct {
	mu    sync.Mutex
	seq   uint64
	turns map[string]*inflightTurn
	notes map[string]*interruptNote
}

type inflightTurn struct {
	tok uint64
	drv agent.Driver
	sid agent.SessionID
}

type interruptNote struct {
	playedText string
	at         time.Time
}

// NewInflightRegistry 构造空注册表。
func NewInflightRegistry() *InflightRegistry {
	return &InflightRegistry{
		turns: make(map[string]*inflightTurn),
		notes: make(map[string]*interruptNote),
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
// 回合子进程(连带其中的工具执行),并记录打断备注供下一回合注入。
//
// playedText 是设备上报的"用户实际听到的最后内容"(可空)。
// 返回 found=false 表示没有 in-flight turn(本地播放可能仍在跑,设备自行停止);
// err 非 nil 表示找到了 turn 但驱动不支持 Interrupt 或调用失败。
func (r *InflightRegistry) Cancel(deviceID, playedText string) (found bool, err error) {
	r.mu.Lock()
	t, ok := r.turns[deviceID]
	if !ok && len(r.turns) == 1 {
		for k, v := range r.turns {
			deviceID, t, ok = k, v, true
		}
	}
	if ok {
		// 无论 Interrupt 成败都记备注:用户的打断意图是事实。
		r.notes[deviceID] = &interruptNote{playedText: strings.TrimSpace(playedText), at: time.Now()}
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

// NoteInterruption 在没有 in-flight turn 的情况下也记一条打断备注
// (例如设备只是打断了本地 TTS 播放,agent 回合早已结束)。
func (r *InflightRegistry) NoteInterruption(deviceID, playedText string) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.notes[deviceID] = &interruptNote{playedText: strings.TrimSpace(playedText), at: time.Now()}
}

// noteTTL 之外的打断备注不再注入:隔了很久的下一轮对话,旧打断已无意义。
const noteTTL = 30 * time.Minute

// ConsumePromptNote 取出并清除 deviceID 的待注入打断备注,渲染成下一回合
// prompt 的前缀段。没有备注(或备注过期)返回 ""。
func (r *InflightRegistry) ConsumePromptNote(deviceID string) string {
	r.mu.Lock()
	n, ok := r.notes[deviceID]
	if !ok && len(r.notes) == 1 {
		for k, v := range r.notes {
			deviceID, n, ok = k, v, true
		}
	}
	if ok {
		delete(r.notes, deviceID)
	}
	r.mu.Unlock()
	if !ok || time.Since(n.at) > noteTTL {
		return ""
	}
	if n.playedText != "" {
		return fmt.Sprintf("[系统提示:你上一条回复在「%s」之后被用户按键打断,其余内容用户没有听到;当时若有正在执行的操作也已被终止。请结合用户接下来的话继续,不要假设上一条回复已完整送达。]", n.playedText)
	}
	return "[系统提示:你上一条回复被用户按键打断,用户可能没有听到全部内容;当时若有正在执行的操作也已被终止。请结合用户接下来的话继续,不要假设上一条回复已完整送达。]"
}
