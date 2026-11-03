package outputguard

import (
	"errors"
	"math"
	"sync"
)

const (
	MaxRequestEvents   = 131072
	MaxRequestChannels = 128
	TriggeredCode      = "model_output_guard_triggered"
)

type Config struct {
	Enabled               bool
	MaxToolArgumentBytes  int64
	MaxToolArgumentDeltas int
	MaxChannelBytes       int64
	MaxRequestBytes       int64
}

type RequestGuard struct {
	mu            sync.RWMutex
	cfg           Config
	requestEvents int
	requestBytes  int64
	channels      map[string]*ChannelGuard
	aliasMap      map[string]string
	cachedChanID  string
	cachedAliasID string
	cachedTarget  *ChannelGuard
	tripped       bool
	trippedReason string
}

func NewRequestGuard(cfg Config) *RequestGuard {
	return &RequestGuard{
		cfg:      cfg,
		channels: make(map[string]*ChannelGuard),
		aliasMap: make(map[string]string),
	}
}

func (g *RequestGuard) IsTripped() bool {
	g.mu.RLock()
	defer g.mu.RUnlock()
	return g.tripped
}

func (g *RequestGuard) TrippedReason() string {
	g.mu.RLock()
	defer g.mu.RUnlock()
	return g.trippedReason
}

func (g *RequestGuard) ActiveChannelCount() int {
	g.mu.RLock()
	defer g.mu.RUnlock()
	return len(g.channels)
}

func (g *RequestGuard) tripLocked(reason string) error {
	if !g.tripped {
		g.tripped = true
		g.trippedReason = reason
	}
	return errors.New(g.trippedReason)
}

func (g *RequestGuard) resolveChannelLocked(channelID, aliasID string, kind ChannelKind) (*ChannelGuard, error) {
	if g.cachedTarget != nil && g.cachedChanID == channelID && g.cachedAliasID == aliasID {
		return g.cachedTarget, nil
	}

	// 晚到 ID 合并与双向映射机制 (Spec §4.3 & 审查反馈 M2 修复)：
	// 真实 Responses 流式 delta 初期通常仅包含 item_id（如 item_call_1），而在 output_item.done 时才提供 call_id。
	// 若分别创建会导致同一工具调用被分配两个 channel。
	var primaryKey string
	if channelID != "" && g.aliasMap[channelID] != "" {
		primaryKey = g.aliasMap[channelID]
	} else if aliasID != "" && g.aliasMap[aliasID] != "" {
		primaryKey = g.aliasMap[aliasID]
	} else if channelID != "" && g.channels[channelID] != nil {
		primaryKey = channelID
	} else if aliasID != "" && g.channels[aliasID] != nil {
		primaryKey = aliasID
	} else if channelID != "" {
		primaryKey = channelID
	} else if aliasID != "" {
		primaryKey = aliasID
	} else {
		primaryKey = "default"
	}

	// 建立双向 alias 映射，确保后续无论通过 channelID 还是 aliasID 查找均指向同一主 ChannelGuard 实例
	if channelID != "" {
		g.aliasMap[channelID] = primaryKey
	}
	if aliasID != "" {
		g.aliasMap[aliasID] = primaryKey
	}

	cg, exists := g.channels[primaryKey]
	if !exists {
		// 步骤 2: Channel 数量检查 (Spec §5.3)
		if len(g.channels)+1 > MaxRequestChannels {
			return nil, g.tripLocked("channel_count")
		}
		maxBytes := g.cfg.MaxChannelBytes
		if kind == KindToolArguments {
			maxBytes = g.cfg.MaxToolArgumentBytes
		}
		cg = NewChannelGuard(primaryKey, kind, maxBytes, g.cfg.MaxToolArgumentDeltas)
		g.channels[primaryKey] = cg
	}

	g.cachedChanID = channelID
	g.cachedAliasID = aliasID
	g.cachedTarget = cg

	return cg, nil
}

// RecordEvent 严格对每上游业务事件执行 1 次计数（步骤 1: request_events 检查）
func (g *RequestGuard) RecordEvent() error {
	g.mu.Lock()
	defer g.mu.Unlock()

	if !g.cfg.Enabled {
		return nil
	}
	if g.tripped {
		return errors.New(g.trippedReason)
	}

	g.requestEvents++
	if g.requestEvents > MaxRequestEvents {
		return g.tripLocked("request_events")
	}
	return nil
}

// ObserveAppend 严格执行 Spec §5.3 优先级流水线（先步骤 1 事件计数，再步骤 2~7 判定，首 reason 冻结）
func (g *RequestGuard) ObserveAppend(channelID string, aliasID string, kind ChannelKind, delta []byte) error {
	g.mu.Lock()
	defer g.mu.Unlock()

	if !g.cfg.Enabled {
		return nil
	}
	if g.tripped {
		return errors.New(g.trippedReason)
	}

	// 步骤 1: 事件预算检查
	g.requestEvents++
	if g.requestEvents > MaxRequestEvents {
		return g.tripLocked("request_events")
	}

	return g.observeAppendLocked(channelID, aliasID, kind, delta)
}

// ObserveAppendNoEvent 仅执行步骤 2~7 判定，不重复累加 requestEvents（供 ObserveEvent 批处理调用）
func (g *RequestGuard) ObserveAppendNoEvent(channelID string, aliasID string, kind ChannelKind, delta []byte) error {
	g.mu.Lock()
	defer g.mu.Unlock()

	if !g.cfg.Enabled {
		return nil
	}
	if g.tripped {
		return errors.New(g.trippedReason)
	}

	return g.observeAppendLocked(channelID, aliasID, kind, delta)
}

func (g *RequestGuard) observeAppendLocked(channelID string, aliasID string, kind ChannelKind, delta []byte) error {
	// 步骤 2: Channel 检查
	cg, err := g.resolveChannelLocked(channelID, aliasID, kind)
	if err != nil {
		return err
	}

	// 步骤 3 & 4: Delta 计数与语义字节高水位检查
	growth, cost, err := cg.CheckAppend(delta)
	if err != nil {
		return g.tripLocked(err.Error())
	}

	// 步骤 4: 请求级语义字节检查（必须先于步骤 6 扫描预算判定，且含 int64 防溢出）
	if growth > 0 && g.requestBytes > math.MaxInt64-growth {
		return g.tripLocked("request_bytes")
	}
	if g.requestBytes+growth > g.cfg.MaxRequestBytes {
		return g.tripLocked("request_bytes")
	}

	// 步骤 6: 扫描预算扣减与检查 (Spec §5.3 步骤 6，在步骤 4 request_bytes 之后执行)
	if cg.remainingScanBudget < cost {
		if cg.kind == KindToolArguments {
			return g.tripLocked("tool_argument_bytes")
		}
		return g.tripLocked("channel_bytes")
	}

	// 步骤 5, 6, 7: 提交高水位并执行周期扫描
	g.requestBytes += growth
	if err := cg.CommitAppend(delta, growth, cost); err != nil {
		return g.tripLocked(err.Error())
	}

	return nil
}

// ObserveSnapshot 严格执行 Spec §5.3 快照判定流水线（先步骤 1 事件计数，再步骤 2~7 判定）
func (g *RequestGuard) ObserveSnapshot(channelID string, aliasID string, kind ChannelKind, snapshot []byte, terminalStage string) error {
	g.mu.Lock()
	defer g.mu.Unlock()

	if !g.cfg.Enabled {
		return nil
	}
	if g.tripped {
		return errors.New(g.trippedReason)
	}

	// 步骤 1: 事件预算检查
	g.requestEvents++
	if g.requestEvents > MaxRequestEvents {
		return g.tripLocked("request_events")
	}

	return g.observeSnapshotLocked(channelID, aliasID, kind, snapshot, terminalStage)
}

// ObserveSnapshotNoEvent 仅执行步骤 2~7 判定，不重复累加 requestEvents（供 ObserveEvent 批处理调用）
func (g *RequestGuard) ObserveSnapshotNoEvent(channelID string, aliasID string, kind ChannelKind, snapshot []byte, terminalStage string) error {
	g.mu.Lock()
	defer g.mu.Unlock()

	if !g.cfg.Enabled {
		return nil
	}
	if g.tripped {
		return errors.New(g.trippedReason)
	}

	return g.observeSnapshotLocked(channelID, aliasID, kind, snapshot, terminalStage)
}

func (g *RequestGuard) observeSnapshotLocked(channelID string, aliasID string, kind ChannelKind, snapshot []byte, terminalStage string) error {
	// 步骤 2: Channel 检查
	cg, err := g.resolveChannelLocked(channelID, aliasID, kind)
	if err != nil {
		return err
	}

	// 步骤 4: 语义字节预算检查 (先检查高水位推进与 request_bytes 上限)
	snapLen := int64(len(snapshot))
	var growth int64
	if snapLen > cg.highwater {
		growth = snapLen - cg.highwater
	}
	if snapLen > cg.maxBytes {
		if cg.kind == KindToolArguments {
			return g.tripLocked("tool_argument_bytes")
		}
		return g.tripLocked("channel_bytes")
	}
	if growth > 0 && g.requestBytes > math.MaxInt64-growth {
		return g.tripLocked("request_bytes")
	}
	if g.requestBytes+growth > g.cfg.MaxRequestBytes {
		return g.tripLocked("request_bytes")
	}

	// 步骤 5 & 6: 资格/等价性检查与扫描预算扣减
	growth, cost, _, err := cg.CheckSnapshot(snapshot, terminalStage)
	if err != nil {
		if errors.Is(err, ErrScanBudgetExhausted) {
			if cg.kind == KindToolArguments {
				return g.tripLocked("tool_argument_bytes")
			}
			return g.tripLocked("channel_bytes")
		}
		return g.tripLocked(err.Error())
	}

	// 步骤 7: 提交高水位与扫描
	g.requestBytes += growth
	if err := cg.CommitSnapshot(snapshot, growth, cost, terminalStage); err != nil {
		return g.tripLocked(err.Error())
	}

	return nil
}

// EndChannel 在关闭 channel 前执行尾检，释放窗口内存，累计 channel 映射不返还
func (g *RequestGuard) EndChannel(channelID string) error {
	g.mu.Lock()
	defer g.mu.Unlock()

	targetKey := channelID
	if alias := g.aliasMap[channelID]; alias != "" {
		targetKey = alias
	}

	if cg, exists := g.channels[targetKey]; exists {
		// 先执行尾检 (Spec §5.2 规则 6)
		if err := cg.CheckTail(); err != nil {
			return g.tripLocked(err.Error())
		}
		// 尾检通过后安全释放滑动窗口内存 (<=64KiB)
		cg.ReleaseWindow()
		if g.cachedTarget == cg {
			g.cachedChanID = ""
			g.cachedAliasID = ""
			g.cachedTarget = nil
		}
	}
	return nil
}
