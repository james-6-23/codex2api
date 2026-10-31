package outputguard

import (
	"errors"
	"math"
)

var (
	ErrToolArgumentBytesExceeded = errors.New("tool_argument_bytes")
	ErrChannelBytesExceeded      = errors.New("channel_bytes")
	ErrScanBudgetExhausted       = errors.New("scan_budget_exhausted")
	ErrRepetitionDetected        = errors.New("repetition")
	ErrToolDeltasExceeded        = errors.New("tool_argument_deltas")
)

type ChannelKind string

const (
	KindText          ChannelKind = "text"
	KindThinking      ChannelKind = "thinking"
	KindToolArguments ChannelKind = "tool_arguments"
)

// allowedTerminalTypes 白名单终态类型（Spec §5.3）
var allowedTerminalTypes = map[string]bool{
	"arguments.done":     true,
	"item.done":          true,
	"response.completed": true,
}

// fnv64a 计算 64 位 FNV-1a Hash，用于流水线单遍历比对与基准记录
func fnv64a(data []byte) uint64 {
	var h uint64 = 14695981039346656037
	for _, b := range data {
		h = (h ^ uint64(b)) * 1099511628211
	}
	return h
}

type ChannelGuard struct {
	id                    string
	kind                  ChannelKind
	maxBytes              int64
	maxDeltas             int
	remainingScanBudget   int64
	highwater             int64
	deltaCount            int
	terminalBaselineHash  uint64
	hasBaselineHash       bool
	terminalPermitsUsed   int
	consumedTerminalTypes map[string]bool
	detector              *RepetitionDetector
	isClosed              bool
}

func NewChannelGuard(id string, kind ChannelKind, maxBytes int64, maxDeltas int) *ChannelGuard {
	return &ChannelGuard{
		id:                    id,
		kind:                  kind,
		maxBytes:              maxBytes,
		maxDeltas:             maxDeltas,
		remainingScanBudget:   2 * maxBytes, // 累计扫描预算为 2 * limit (Spec §5.3)
		consumedTerminalTypes: make(map[string]bool),
		detector:              NewRepetitionDetector(),
	}
}

func (c *ChannelGuard) Highwater() int64           { return c.highwater }
func (c *ChannelGuard) DeltaCount() int            { return c.deltaCount }
func (c *ChannelGuard) RemainingScanBudget() int64 { return c.remainingScanBudget }

func (c *ChannelGuard) ReleaseWindow() {
	c.detector = nil
	c.isClosed = true
}

func (c *ChannelGuard) CheckAppend(delta []byte) (growth int64, cost int64, err error) {
	deltaLen := int64(len(delta))
	if c.kind == KindToolArguments && deltaLen > 0 {
		if c.deltaCount+1 > c.maxDeltas {
			return 0, 0, ErrToolDeltasExceeded
		}
	}
	// int64 溢出保护：防止加法溢出翻转为负数绕过预算检查
	if deltaLen > 0 && c.highwater > math.MaxInt64-deltaLen {
		if c.kind == KindToolArguments {
			return 0, 0, ErrToolArgumentBytesExceeded
		}
		return 0, 0, ErrChannelBytesExceeded
	}
	newHighwater := c.highwater + deltaLen
	if newHighwater > c.maxBytes {
		if c.kind == KindToolArguments {
			return 0, 0, ErrToolArgumentBytesExceeded
		}
		return 0, 0, ErrChannelBytesExceeded
	}

	cost = deltaLen
	return deltaLen, cost, nil
}

func (c *ChannelGuard) CommitAppend(delta []byte, growth int64, cost int64) error {
	c.highwater += growth
	if c.kind == KindToolArguments && len(delta) > 0 {
		c.deltaCount++
	}
	if c.remainingScanBudget < cost {
		c.remainingScanBudget = 0
		return ErrScanBudgetExhausted
	}
	c.remainingScanBudget -= cost

	if c.detector != nil && len(delta) > 0 {
		hit, _ := c.detector.Push(delta)
		if hit {
			return ErrRepetitionDetected
		}
	}
	return nil
}

func (c *ChannelGuard) CheckSnapshot(snapshot []byte, terminalStage string) (growth int64, cost int64, skipRepetition bool, err error) {
	snapLen := int64(len(snapshot))
	if snapLen > c.highwater {
		growth = snapLen - c.highwater
	}
	if snapLen > c.maxBytes {
		if c.kind == KindToolArguments {
			return 0, 0, false, ErrToolArgumentBytesExceeded
		}
		return 0, 0, false, ErrChannelBytesExceeded
	}

	// 资格检查：仅白名单终态类型且具备 baseline hash 且许可未耗尽 (Spec §5.3 步骤 5)
	// 合法与非法终态配额判断严格在 hash 前！无资格绝不提前计算 hash。
	isEligible := c.hasBaselineHash &&
		allowedTerminalTypes[terminalStage] &&
		!c.consumedTerminalTypes[terminalStage] &&
		c.terminalPermitsUsed < 3

	if isEligible {
		// 计算 snapshot Hash 比对 baseline hash
		currentHash := fnv64a(snapshot)
		if currentHash == c.terminalBaselineHash {
			// Hash 完全一致，纯终态确认快照零计费放行，跳过步骤 7 重复扫描
			return growth, 0, true, nil
		}
		// Hash 不一致（变体篡改），作废该类型许可，全额计费
		cost = snapLen
	} else {
		// 无免除资格（包括中间替换、首帧终态、重发同类型或超出 3 次配额），严禁提前 Hash，直接全额计费
		cost = snapLen
	}

	if c.remainingScanBudget < cost {
		return 0, 0, false, ErrScanBudgetExhausted
	}

	return growth, cost, false, nil
}

func (c *ChannelGuard) CommitSnapshot(snapshot []byte, growth int64, cost int64, terminalStage string) error {
	c.highwater += growth
	c.remainingScanBudget -= cost

	// 白名单终态类型快照消耗本类型配额
	if allowedTerminalTypes[terminalStage] {
		c.consumedTerminalTypes[terminalStage] = true
		c.terminalPermitsUsed++
	}

	// 终态首帧建立 baseline hash：仅白名单终态帧类型允许在首帧建立 baseline hash (Spec §5.3)
	// 如果之前未建立 baseline 且当前属于白名单类型，在单次遍历中建立 baseline hash
	shouldSetBaseline := !c.hasBaselineHash && allowedTerminalTypes[terminalStage]

	if cost > 0 && c.detector != nil {
		c.detector.Reset()

		if shouldSetBaseline {
			// 单次流水线处理：一次遍历同时流水线计算 FNV-1a Hash 并执行滑动窗口重复检测 (Spec §5.3)
			const fnvOffsetBasis uint64 = 14695981039346656037
			hit, _, finalHash := c.detector.PushWithHash(snapshot, fnvOffsetBasis)
			c.terminalBaselineHash = finalHash
			c.hasBaselineHash = true
			if hit {
				return ErrRepetitionDetected
			}
			hit, _ = c.detector.CheckTail()
			if hit {
				return ErrRepetitionDetected
			}
		} else {
			hit, _ := c.detector.Push(snapshot)
			if hit {
				return ErrRepetitionDetected
			}
			hit, _ = c.detector.CheckTail()
			if hit {
				return ErrRepetitionDetected
			}
		}
	}

	return nil
}

func (c *ChannelGuard) CheckTail() error {
	if c.detector != nil {
		if hit, _ := c.detector.CheckTail(); hit {
			return ErrRepetitionDetected
		}
	}
	return nil
}
