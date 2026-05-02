package controller

import (
	"sync"
	"time"

	"github.com/rs/zerolog/log"
	"github.com/user/live-cdn/internal/common"
)

// LatencyController 自动延迟降级/恢复控制器
//
// 原则：延迟越低越好，档位是自动降级策略而非用户选择。
// 系统始终从 ultra 开始，客户端持续卡顿时自动降档，
// 卡顿消失后逐步恢复到更低延迟。
type LatencyController struct {
	store  *MemoryStore
	mu     sync.Mutex
	stopCh chan struct{}

	// 会话级别的降级状态: sessionID -> degradeState
	sessions map[string]*degradeState
}

type degradeState struct {
	currentMode    common.LatencyMode
	stallStart     time.Time // 卡顿开始时间
	stableStart   time.Time // 无卡顿开始时间
	reportCount    int       // 卡顿报告计数
	override       bool      // 管理员手动 override
}

func NewLatencyController(store *MemoryStore) *LatencyController {
	return &LatencyController{
		store:    store,
		stopCh:   make(chan struct{}),
		sessions: make(map[string]*degradeState),
	}
}

// Start 启动后台检查循环
func (lc *LatencyController) Start() {
	go lc.loop()
}

// Stop 停止
func (lc *LatencyController) Stop() {
	close(lc.stopCh)
}

func (lc *LatencyController) loop() {
	ticker := time.NewTicker(10 * time.Second)
	defer ticker.Stop()

	for {
		select {
		case <-lc.stopCh:
			return
		case <-ticker.C:
			lc.checkAndAdjust()
		}
	}
}

// ProcessQualityReport 处理客户端质量上报，决定是否降档
// 返回值: 是否需要降档, 新的 LatencyMode
func (lc *LatencyController) ProcessQualityReport(sessionID string, stallRate float64, e2eLatency int) (bool, common.LatencyMode) {
	lc.mu.Lock()
	defer lc.mu.Unlock()

	state, ok := lc.sessions[sessionID]
	if !ok {
		// 新会话，默认从 ultra 开始
		state = &degradeState{
			currentMode: common.LatencyUltra,
		}
		lc.sessions[sessionID] = state
	}

	// 管理员 override 的不做自动调整
	if state.override {
		return false, state.currentMode
	}

	// 卡顿检测 (stallRate > 5%)
	if stallRate > 0.05 {
		if state.stallStart.IsZero() {
			state.stallStart = time.Now()
		}
		state.stableStart = time.Time{} // 重置稳定计时
		state.reportCount++

		// 持续卡顿 30 秒 → 降档
		if time.Since(state.stallStart) >= 30*time.Second {
			newMode := lc.downgrade(state.currentMode)
			if newMode != state.currentMode {
				log.Info().
					Str("session", sessionID[:8]).
					Str("from", string(state.currentMode)).
					Str("to", string(newMode)).
					Float64("stall_rate", stallRate).
					Msg("auto downgrade latency mode")
				state.currentMode = newMode
				state.stallStart = time.Time{}
				state.reportCount = 0
				return true, newMode
			}
		}
	} else {
		// 无卡顿
		if state.stableStart.IsZero() {
			state.stableStart = time.Now()
		}
		state.stallStart = time.Time{} // 重置卡顿计时
	}

	return false, state.currentMode
}

// downgrade 降一档
func (lc *LatencyController) downgrade(current common.LatencyMode) common.LatencyMode {
	for _, rule := range common.AutoDowngradeRules {
		if rule.From == current {
			return rule.To
		}
	}
	return current // 已经是最低档
}

// upgrade 升一档
func (lc *LatencyController) upgrade(current common.LatencyMode) common.LatencyMode {
	for _, rule := range common.AutoUpgradeRules {
		if rule.From == current {
			return rule.To
		}
	}
	return current // 已经是最高档
}

// checkAndAdjust 定期检查，恢复稳定会话的延迟档位
func (lc *LatencyController) checkAndAdjust() {
	lc.mu.Lock()
	defer lc.mu.Unlock()

	now := time.Now()
	for sessionID, state := range lc.sessions {
		if state.override {
			continue
		}

		// 无卡顿持续 120 秒 → 升档
		if !state.stableStart.IsZero() && now.Sub(state.stableStart) >= 120*time.Second {
			newMode := lc.upgrade(state.currentMode)
			if newMode != state.currentMode {
				log.Info().
					Str("session", sessionID[:8]).
					Str("from", string(state.currentMode)).
					Str("to", string(newMode)).
					Msg("auto upgrade latency mode (stable)")
				state.currentMode = newMode
				state.stableStart = now // 重新计时
			}
		}
	}

	// 清理过期会话 (超过 1 小时无上报)
	for sessionID, state := range lc.sessions {
		if state.stableStart.IsZero() && state.stallStart.IsZero() {
			delete(lc.sessions, sessionID)
		} else if !state.stableStart.IsZero() && now.Sub(state.stableStart) > time.Hour {
			delete(lc.sessions, sessionID)
		}
	}
}

// SetOverride 管理员手动设置档位
func (lc *LatencyController) SetOverride(sessionID string, mode common.LatencyMode) {
	lc.mu.Lock()
	defer lc.mu.Unlock()

	state, ok := lc.sessions[sessionID]
	if !ok {
		state = &degradeState{currentMode: mode, override: true}
		lc.sessions[sessionID] = state
	} else {
		state.currentMode = mode
		state.override = true
	}
}

// GetSessionMode 获取会话当前档位
func (lc *LatencyController) GetSessionMode(sessionID string) common.LatencyMode {
	lc.mu.Lock()
	defer lc.mu.Unlock()

	if state, ok := lc.sessions[sessionID]; ok {
		return state.currentMode
	}
	return common.LatencyUltra
}
