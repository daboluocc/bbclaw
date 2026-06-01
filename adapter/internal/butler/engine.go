package butler

import (
	"context"
	"strings"

	"github.com/daboluocc/bbclaw/adapter/internal/agent"
	"github.com/daboluocc/bbclaw/adapter/internal/agent/logicalsession"
)

// ButlerDriver 是管家会话固定使用的 agent driver(ADR-021 §1):管家是 per-device 的
// 编排者,跑在 claude-code 上以便加载 workspace 的 CLAUDE.md 人设并经 --mcp-config 派发。
const ButlerDriver = "claude-code"

// Request 是 caller 把自己的 transport 入参规整后传给 butler 的纯数据。
// 差异 #10:deviceId 来源由 caller 决定(LOCAL=URL query;CLOUD=env.DeviceID)。
type Request struct {
	Text             string // 已 TrimSpace;空由 caller 在调用前自行拒绝
	RequestedDriver  string // 已 TrimSpace
	RequestedSession string // 已 TrimSpace;"" / "ls-*" / 裸 cli id
	DeviceID         string
}

// Deps 是构造 Engine 必须注入的依赖(每 caller 各传一份)。
type Deps struct {
	Router   *agent.Router
	Sessions *logicalsession.Manager // 可为 nil(禁用 logical 路径)
	Registry SessionRegistry         // 各 transport 适配自己的注册表
	Sink     EventSink               // 各 transport 序列化
	Hooks    Hooks
	Policy   Policy
	Metrics  MetricsSink
	Log      Logger

	// ResolveActiveModel 注入:两 caller 各传自己的。driver 为驱动名,返回持久化
	// active_model 或 ""。butler 不缓存其结果以保留 ADR-016 mid-session 语义。
	ResolveActiveModel func(driver string) string

	// SystemPrompt 注入(ADR-018 §3):据 logical cwd 构造每轮的系统提示,经
	// StartOpts.SystemPrompt 下达给 driver(claudecode → --append-system-prompt)。
	// nil = 不注入。两 caller 通常都传 butler.DeviceSystemPrompt。
	SystemPrompt func(cwd string) string

	// StartCtx 是传给 drv.Start 的 ctx(差异 #9)。
	//   LOCAL  = s.agentCtx(长生命周期)
	//   CLOUD  = 请求 ctx
	StartCtx context.Context

	// ButlerMCPConfig 是管家会话的 --mcp-config 文件路径(ADR-021 §2)。仅当本轮解析到的
	// logical session 的 Role 为 logicalsession.RoleButler 时,才经 StartOpts.MCPConfig
	// 下达给 driver(claudecode → --mcp-config),让管家可派发 worker。worker 会话不带。
	// "" = 不注入(非管家路径或未配置 mcp-server)。
	ButlerMCPConfig string
}

// Engine 持有不变的依赖;RunTurn 每次调用驱动一个完整 turn。
type Engine struct{ d Deps }

// NewEngine 构造一个 Engine。
func NewEngine(d Deps) *Engine { return &Engine{d: d} }

// Result 汇总 turn 结束时的可观察统计,供 caller 做收尾决策(CLOUD reply、日志)。
type Result struct {
	LogicalID     string // "" 表示未用 logical
	UsingLogical  bool
	DriverName    string
	FinalSID      string // 最终 cli sid(string)
	VisibleID     string // 发给设备的 id:usingLogical ? logicalID : FinalSID
	TurnEnded     bool
	TextCount     int
	ErrorCount    int
	LastText      string
	LastError     string
	SendErr       error // 最后一次 attempt 的 drv.Send 返回
	ChannelClosed bool  // events 通道被 driver 关闭
}

func (d Deps) mTurnStart() {
	if d.Metrics != nil {
		d.Metrics.TurnStart()
	}
}

func (d Deps) mResumeSkipped() {
	if d.Metrics != nil {
		d.Metrics.ResumeSkippedMissing()
	}
}

func (d Deps) mRetry() {
	if d.Metrics != nil {
		d.Metrics.SessionNotFoundRetry()
	}
}

func (d Deps) mTurnDone(turnEnded bool, textCount, errorCount int) {
	if d.Metrics != nil {
		d.Metrics.TurnDone(turnEnded, textCount, errorCount)
	}
}

func (d Deps) resolveModel(driver string) string {
	if d.ResolveActiveModel == nil {
		return ""
	}
	return d.ResolveActiveModel(driver)
}

func (d Deps) buildSystemPrompt(cwd string) string {
	if d.SystemPrompt == nil {
		return ""
	}
	return d.SystemPrompt(cwd)
}

// RunTurn 跑完整个 turn 骨架(解析→主动 resume 校验→attempt 循环→收尾)。
//
// turnCtx 是消费 events 的 ctx(差异 #9):LOCAL=r.Context();CLOUD=请求 ctx。
//
// 返回 *CodedError(解析期或运行期);成功返回 nil。caller 据 PreStream 决定是否
// 回 4xx JSON(此时 RunTurn 保证一帧都没经 Sink 发出)还是已经走了流式。CLOUD 的
// caller 把非 nil 错误转成自己的 Go-error/reply。
//
// 注意:RunTurn 内部不发 CLOUD-only 的收尾 reply(差异 #7),那由 Hooks.OnFinalReply
// 在 turn 末尾据 Result 自行 emit。
func (e *Engine) RunTurn(turnCtx context.Context, req Request) (*Result, error) {
	d := e.d
	maxAttempts := d.Policy.maxAttempts()

	// ── 1) driver 解析(差异 #2 PreStream) ──
	var (
		drv        agent.Driver
		driverName string
	)
	if req.RequestedDriver != "" {
		var ok bool
		drv, ok = d.Router.Get(req.RequestedDriver)
		if !ok {
			return nil, &CodedError{
				Code:       "UNKNOWN_DRIVER",
				Detail:     "driver not registered: " + req.RequestedDriver,
				PreStream:  true,
				HTTPStatus: 400,
			}
		}
		driverName = req.RequestedDriver
	} else {
		drv = d.Router.Default()
		if drv == nil {
			return nil, ErrNotConfigured
		}
		driverName = drv.Name()
	}

	// ── 2) 逻辑会话解析 ──
	var (
		logicalID         logicalsession.ID
		resumeFromLogical string
		logicalCwd        string
		logicalRole       string
		usingLogical      bool

		sid   agent.SessionID
		isNew bool
	)

	if d.Sessions != nil {
		switch {
		case strings.HasPrefix(req.RequestedSession, "ls-"):
			ls, ok := d.Sessions.Get(logicalsession.ID(req.RequestedSession))
			if !ok {
				// ls- 前缀但未命中 → 流式 error 帧(差异 #2 PreStream=false)。
				d.Sink.EmitError("UNKNOWN_LOGICAL_SESSION", "logical session not found: "+req.RequestedSession, false)
				return nil, &CodedError{Code: "UNKNOWN_LOGICAL_SESSION", PreStream: false}
			}
			logicalID = ls.ID
			resumeFromLogical = ls.CLISessionID
			logicalCwd = ls.Cwd
			logicalRole = ls.Role
			usingLogical = true
			if resumeFromLogical != "" {
				if dn, esid, found := d.Registry.Get(resumeFromLogical); found {
					if req.RequestedDriver != "" && req.RequestedDriver != dn {
						return nil, &CodedError{
							Code:       "SESSION_DRIVER_MISMATCH",
							Detail:     "sessionId is pinned to driver=" + dn + ", request asked for driver=" + req.RequestedDriver,
							PreStream:  true,
							HTTPStatus: 400,
						}
					}
					pinned, ok2 := d.Router.Get(dn)
					if !ok2 {
						return nil, &CodedError{
							Code:       "SESSION_UNREGISTERED_DRIVER",
							Detail:     "session references unregistered driver: " + dn,
							PreStream:  true,
							HTTPStatus: 500,
						}
					}
					drv = pinned
					driverName = dn
					sid = esid
					isNew = false
				}
			}
		case req.RequestedSession == "":
			// 空 id:差异 #3 ReuseWindow。仅 LOCAL(Policy.ReuseWindow>0)在 Create
			// 之前先尝试 FindRecent 复用。
			reused := false
			if d.Policy.ReuseWindow > 0 {
				if recent := d.Sessions.FindRecent(req.DeviceID, driverName, d.Policy.ReuseWindow); recent != nil {
					logicalID = recent.ID
					resumeFromLogical = recent.CLISessionID
					logicalCwd = recent.Cwd
					logicalRole = recent.Role
					usingLogical = true
					if resumeFromLogical != "" {
						if dn, esid, found := d.Registry.Get(resumeFromLogical); found {
							if _, ok2 := d.Router.Get(dn); ok2 {
								drv = mustGet(d.Router, dn, drv)
								driverName = dn
								sid = esid
								isNew = false
							}
						}
					}
					if d.Log != nil {
						d.Log.Infof("agent: reusing recent logical=%s device=%s driver=%s", logicalID, req.DeviceID, driverName)
					}
					reused = true
				}
			}
			if !reused {
				ls, err := d.Sessions.Create(req.DeviceID, driverName, "", "")
				if err != nil {
					return nil, &CodedError{
						Code:       "CREATE_SESSION_FAILED",
						Detail:     err.Error(),
						PreStream:  true,
						HTTPStatus: 500,
						Err:        err,
					}
				}
				logicalID = ls.ID
				logicalCwd = ls.Cwd
				usingLogical = true
			}
		}
	}

	// ── 裸 CLI id 分支(非空、无 ls- 前缀、未走 logical) ── 差异 #4
	if !usingLogical && req.RequestedSession != "" {
		if !d.Policy.AllowBareCLIID {
			return nil, &CodedError{
				Code:       "INVALID_SESSION_ID",
				Detail:     "sessionId must be a logical id (ls- prefix) or empty; bare CLI session ids are no longer accepted — please upgrade firmware to v0.5+",
				PreStream:  true,
				HTTPStatus: 400,
			}
		}
		// CLOUD legacy path:registry 命中则 pin,否则当 ResumeID 由 attempt0 用。
		if dn, esid, found := d.Registry.Get(req.RequestedSession); found {
			if req.RequestedDriver != "" && req.RequestedDriver != dn {
				return nil, &CodedError{
					Code:      "SESSION_DRIVER_MISMATCH",
					Detail:    "want=" + req.RequestedDriver + ",have=" + dn,
					PreStream: true,
				}
			}
			pinned, ok2 := d.Router.Get(dn)
			if !ok2 {
				return nil, &CodedError{
					Code:      "SESSION_UNREGISTERED_DRIVER",
					Detail:    dn,
					PreStream: true,
				}
			}
			drv = pinned
			driverName = dn
			sid = esid
		}
	}

	d.mTurnStart()

	// ── 3) 主动 resume 校验 ──
	if resumeFromLogical != "" {
		if checker, ok := drv.(agent.CLISessionChecker); ok {
			if !checker.CLISessionExists(resumeFromLogical) {
				if d.Log != nil {
					d.Log.Infof("agent: resume target missing on disk, skipping resume cli=%s logical=%s", resumeFromLogical, logicalID)
				}
				d.mResumeSkipped()
				resumeFromLogical = ""
			}
		}
	}

	// ── 4) attempt 循环 ──
	var (
		textCount     int
		errorCount    int
		lastError     string
		lastText      string
		turnEnded     bool
		sendErr       error
		channelClosed bool
	)
	for attempt := 0; attempt < maxAttempts; attempt++ {
		textCount = 0
		errorCount = 0
		lastError = ""
		lastText = ""
		turnEnded = false
		sendErr = nil
		channelClosed = false
		sessionNotFound := false

		if sid == "" {
			startOpts := agent.StartOpts{}
			if logicalCwd != "" {
				startOpts.Cwd = logicalCwd
			}
			startOpts.Model = d.resolveModel(drv.Name())
			startOpts.SystemPrompt = d.buildSystemPrompt(logicalCwd)
			// 仅管家会话(Role=butler)带 --mcp-config,让它能派发 worker(ADR-021 §2);
			// worker / 普通会话不带。driver 不支持 MCP 时忽略(契约同 Model)。
			if logicalRole == logicalsession.RoleButler && d.ButlerMCPConfig != "" {
				startOpts.MCPConfig = d.ButlerMCPConfig
			}
			isResumeAttempt := false
			if attempt == 0 {
				switch {
				case usingLogical:
					if resumeFromLogical != "" {
						startOpts.ResumeID = resumeFromLogical
						isResumeAttempt = true
					}
				case req.RequestedSession != "":
					startOpts.ResumeID = req.RequestedSession
					isResumeAttempt = true
				}
			}
			newSid, err := drv.Start(d.StartCtx, startOpts)
			if err != nil {
				if d.Policy.EmitStartFailedFrame {
					d.Sink.EmitError("AGENT_START_FAILED", err.Error(), true)
				}
				return nil, &CodedError{Code: "AGENT_START_FAILED", Detail: err.Error(), PreStream: false, Err: err}
			}
			sid = newSid
			isNew = !isResumeAttempt
			d.Registry.Put(string(sid), driverName, sid)
			if usingLogical && logicalID != "" {
				if err := d.Sessions.UpdateCLISessionID(logicalID, string(sid)); err != nil && d.Log != nil {
					d.Log.Warnf("agent: UpdateCLISessionID logical=%s cli=%s err=%v", logicalID, sid, err)
				}
			}
		} else {
			// 差异 #11:sid!="" else 分支 setState(running)。CLOUD 实现为 no-op。
			d.Registry.SetState(string(sid), "running")
		}

		// 发 session 帧;visibleID = usingLogical ? logicalID : sid。
		visibleSessionID := string(sid)
		if usingLogical && logicalID != "" {
			visibleSessionID = string(logicalID)
		}
		if !d.Sink.EmitSession(visibleSessionID, isNew, driverName) {
			return nil, nil
		}
		d.Registry.Touch(string(sid))

		if d.Log != nil {
			if attempt == 0 {
				d.Log.Infof("phase=agent_start driver=%s sid=%s is_new=%v cwd=%q text_chars=%d", driverName, sid, isNew, logicalCwd, len(req.Text))
			} else {
				d.Log.Warnf("phase=agent_retry driver=%s sid=%s attempt=%d reason=SESSION_NOT_FOUND", driverName, sid, attempt)
			}
		}

		events := drv.Events(sid)
		sendErrCh := make(chan error, 1)
		curSid := sid
		// ADR-016:Send 前推入最新 active_model,让 mid-session 切换本轮生效。
		if mu, ok := drv.(agent.ModelUpdater); ok {
			_ = mu.UpdateModel(curSid, d.resolveModel(drv.Name()))
		}
		go func() { sendErrCh <- drv.Send(curSid, req.Text) }()

	loop:
		for {
			select {
			case <-turnCtx.Done():
				// 复刻两边:消费循环 ctx done 直接返回(不排空 sendErrCh,与原实现一致)。
				// 返回 turnCtx.Err():CLOUD caller 直接把它当 Go-error 返回;LOCAL caller
				// 把它当作「流已结束」忽略(流已开始,无法再回 JSON)。
				if d.Log != nil {
					d.Log.Warnf("agent: ctx done driver=%s sid=%s", driverName, sid)
				}
				return nil, turnCtx.Err()
			case ev, ok := <-events:
				if !ok {
					d.Registry.Drop(string(sid))
					channelClosed = true
					break loop
				}
				switch ev.Type {
				case agent.EvSessionInit:
					if usingLogical && logicalID != "" && ev.Text != "" {
						if err := d.Sessions.UpdateCLISessionID(logicalID, ev.Text); err != nil && d.Log != nil {
							d.Log.Warnf("agent: UpdateCLISessionID (init) logical=%s cli=%s err=%v", logicalID, ev.Text, err)
						}
					}
				case agent.EvText:
					textCount++
					lastText = ev.Text
				case agent.EvError:
					errorCount++
					lastError = ev.Text
					if strings.HasPrefix(ev.Text, "SESSION_NOT_FOUND") {
						sessionNotFound = true
					}
					// 仅在不打算透明重试时广播 error 状态(差异 #5)。
					if !sessionNotFound || attempt+1 >= maxAttempts {
						d.Registry.SetState(string(sid), "error")
						bSID := string(sid)
						if usingLogical && logicalID != "" {
							bSID = string(logicalID)
						}
						if d.Hooks.OnStateChange != nil {
							d.Hooks.OnStateChange(bSID, "error", ev.Text)
						}
					}
				case agent.EvTurnEnd:
					turnEnded = true
				}

				// EvSessionInit 一律不外发(两边一致)。
				if ev.Type == agent.EvSessionInit {
					continue
				}

				// turn_end 外发分歧(差异 EmitTurnEndFrame)。
				// LOCAL:发一帧 NDJSON turn_end(写失败则直接 return,复刻原 writeAgentEvent
				// 失败路径,不排空 sendErrCh)再 break。CLOUD:不外发,直接 break。
				if ev.Type == agent.EvTurnEnd {
					if d.Policy.EmitTurnEndFrame {
						if !d.Sink.EmitEvent(ev) {
							return nil, nil
						}
					}
					break loop
				}

				// 在将要重试的 attempt 上抑制 SESSION_NOT_FOUND error 帧。
				if ev.Type == agent.EvError && sessionNotFound && attempt+1 < maxAttempts {
					continue
				}
				if !d.Sink.EmitEvent(ev) {
					// 客户端断开:复刻 LOCAL `return`(不排空 sendErrCh)。
					return nil, nil
				}
			}
		}

		sendErr = <-sendErrCh
		if sendErr != nil && d.Log != nil {
			d.Log.Errorf("phase=agent_send_failed driver=%s sid=%s err=%v attempt=%d", driverName, sid, sendErr, attempt)
		}

		if sessionNotFound && attempt+1 < maxAttempts {
			d.Registry.Drop(string(sid))
			sid = ""
			isNew = false
			d.mRetry()
			continue
		}
		break
	}

	// ── 5) 收尾 ──
	d.Registry.Touch(string(sid))

	if !channelClosed {
		d.Registry.SetState(string(sid), "completed")
		cSID := string(sid)
		if usingLogical && logicalID != "" {
			cSID = string(logicalID)
		}
		if d.Hooks.OnStateChange != nil {
			d.Hooks.OnStateChange(cSID, "completed", lastText)
		}
	}

	if usingLogical && logicalID != "" && turnEnded {
		if err := d.Sessions.Touch(logicalID); err != nil && d.Log != nil {
			d.Log.Warnf("agent: Touch logical=%s err=%v", logicalID, err)
		}
		if d.Policy.AutoTitle {
			if ls, ok := d.Sessions.Get(logicalID); ok && ls.Title == "" {
				title := truncateRunes(req.Text, 20)
				if title != "" {
					if err := d.Sessions.SetTitle(logicalID, title); err != nil && d.Log != nil {
						d.Log.Warnf("agent: auto-title logical=%s err=%v", logicalID, err)
					}
				}
			}
		}
	}

	visibleID := string(sid)
	if usingLogical && logicalID != "" {
		visibleID = string(logicalID)
	}

	if turnEnded && d.Hooks.OnTurnComplete != nil {
		notifType := "turn_end"
		if errorCount > 0 && textCount == 0 {
			notifType = "error"
		}
		d.Hooks.OnTurnComplete(Notification{
			SessionID: visibleID,
			Driver:    driverName,
			Type:      notifType,
			Preview:   lastText,
		})
	}

	// 收尾指标由 caller 经 MetricsSink.TurnDone 据原始信号自行判定名与分支
	// (LOCAL agent_message_ok/error_only;CLOUD agent_proxy_message_ok/error)。
	d.mTurnDone(turnEnded, textCount, errorCount)
	if errorCount > 0 && textCount == 0 {
		if d.Log != nil {
			d.Log.Warnf("phase=agent_done_error_only driver=%s sid=%s errors=%d last=%q", driverName, sid, errorCount, lastError)
		}
	} else {
		if d.Log != nil {
			d.Log.Infof("phase=agent_done driver=%s sid=%s text=%d errors=%d", driverName, sid, textCount, errorCount)
		}
	}

	res := &Result{
		LogicalID:     string(logicalID),
		UsingLogical:  usingLogical,
		DriverName:    driverName,
		FinalSID:      string(sid),
		VisibleID:     visibleID,
		TurnEnded:     turnEnded,
		TextCount:     textCount,
		ErrorCount:    errorCount,
		LastText:      lastText,
		LastError:     lastError,
		SendErr:       sendErr,
		ChannelClosed: channelClosed,
	}

	if d.Hooks.OnFinalReply != nil {
		d.Hooks.OnFinalReply(res)
	}
	return res, nil
}

// mustGet 是 FindRecent pin 分支的小助手:已确认 dn 在 router 中,取出对应 driver;
// 极少数竞态下回退到 fallback(原行为等价)。
func mustGet(r *agent.Router, dn string, fallback agent.Driver) agent.Driver {
	if drv, ok := r.Get(dn); ok {
		return drv
	}
	return fallback
}
