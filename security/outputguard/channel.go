package outputguard

import (
	"crypto/sha256"
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

// postEndDetector 在 channel 结束后继续对后续 append/snapshot 做有界周期检测。
// 它只保留 8 KiB 环形窗口与检查点游标（不再保留 24 KiB 的线性化/前缀辅助数组），
// 真正求周期时借用临时检测器完成。这样 EndChannel 后仍能阻止迟到的重复数据，
// 同时避免为每个已关闭 channel 长期持有完整 RepetitionDetector 的内存。
type postEndDetector struct {
	window                [WindowSizeBytes]byte
	count                 int64 // 累计写入总字节数（与关闭前检测器连续）
	head                  int   // ring buffer 写入游标 (0..WindowSizeBytes-1)
	lastCheckedCheckpoint int64
}

// newPostEndDetector 从关闭前的完整检测器继承尾部窗口与检查点状态，
// 使结束后的窗口判定与"从未释放窗口"时保持等价语义。
func newPostEndDetector(from *RepetitionDetector) *postEndDetector {
	d := &postEndDetector{}
	if from != nil {
		d.window = from.window
		d.count = from.count
		d.head = from.head
		d.lastCheckedCheckpoint = from.lastCheckedCheckpoint
	}
	return d
}

func (d *postEndDetector) Reset() {
	d.count = 0
	d.head = 0
	d.lastCheckedCheckpoint = 0
}

func (d *postEndDetector) writeChunk(chunk []byte) {
	n := len(chunk)
	if n == 0 {
		return
	}
	tailSpace := WindowSizeBytes - d.head
	if n <= tailSpace {
		copy(d.window[d.head:], chunk)
		d.head += n
		if d.head == WindowSizeBytes {
			d.head = 0
		}
	} else {
		copy(d.window[d.head:], chunk[:tailSpace])
		copy(d.window[:n-tailSpace], chunk[tailSpace:])
		d.head = n - tailSpace
	}
	d.count += int64(n)
}

func (d *postEndDetector) bytesNeededToNextCheckpoint() int {
	if d.count < WindowSizeBytes {
		return int(WindowSizeBytes - d.count)
	}
	nextCheckpoint := d.lastCheckedCheckpoint + CheckpointStepBytes
	if nextCheckpoint <= d.count {
		nextCheckpoint = d.count - (d.count % CheckpointStepBytes) + CheckpointStepBytes
	}
	return int(nextCheckpoint - d.count)
}

func (d *postEndDetector) advanceCheckpoint() {
	d.lastCheckedCheckpoint = d.count - (d.count % CheckpointStepBytes)
}

// Push 与 RepetitionDetector.Push 保持相同的检查点语义，仅在检查点对最新窗口求周期。
func (d *postEndDetector) Push(data []byte) (hit bool, period int) {
	for len(data) > 0 {
		needed := d.bytesNeededToNextCheckpoint()

		if len(data) < needed {
			d.writeChunk(data)
			return false, 0
		}

		d.writeChunk(data[:needed])
		data = data[needed:]
		d.advanceCheckpoint()
		if ok, p := d.checkCurrentWindow(); ok {
			return true, p
		}
	}
	return false, 0
}

// CheckTail 与 RepetitionDetector.CheckTail 语义一致：不足一个完整窗口时不判定。
func (d *postEndDetector) CheckTail() (hit bool, period int) {
	if d.count < WindowSizeBytes {
		return false, 0
	}
	return d.checkCurrentWindow()
}

// checkCurrentWindow 将环形窗口线性化后复用 RepetitionDetector 的 KMP 周期判定，
// 避免复制重复检测算法。临时检测器为栈上固定大小对象，不产生长期堆占用。
func (d *postEndDetector) checkCurrentWindow() (bool, int) {
	if d.count < WindowSizeBytes {
		return false, 0
	}
	var linear [WindowSizeBytes]byte
	tailLen := WindowSizeBytes - d.head
	copy(linear[:tailLen], d.window[d.head:])
	copy(linear[tailLen:], d.window[:d.head])

	var scratch RepetitionDetector
	return scratch.Push(linear[:])
}

type ChannelGuard struct {
	id                    string
	kind                  ChannelKind
	maxBytes              int64
	maxDeltas             int
	remainingScanBudget   int64
	highwater             int64
	deltaCount            int
	terminalBaselineHash  [sha256.Size]byte
	hasBaselineHash       bool
	terminalPermitsUsed   int
	consumedTerminalTypes map[string]bool
	detector              *RepetitionDetector
	postEnd               *postEndDetector
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

// ReleaseWindow 释放完整检测窗口，但保留轻量尾部检测器：
// 已关闭 channel 之后迟到的 append/snapshot 仍必须继续接受有界周期检测。
func (c *ChannelGuard) ReleaseWindow() {
	if c.isClosed {
		return
	}
	c.postEnd = newPostEndDetector(c.detector)
	c.detector = nil
	c.isClosed = true
}

// resetScanners 在整帧快照扫描前重置窗口状态（快照代表完整状态，不与历史 append 拼接）。
func (c *ChannelGuard) resetScanners() {
	if c.detector != nil {
		c.detector.Reset()
	}
	if c.postEnd != nil {
		c.postEnd.Reset()
	}
}

// scanPush 将数据送入当前可用的检测器；关闭后自动切换到轻量尾部检测器。
func (c *ChannelGuard) scanPush(data []byte) (bool, int) {
	if c.detector != nil {
		return c.detector.Push(data)
	}
	if c.postEnd != nil {
		return c.postEnd.Push(data)
	}
	return false, 0
}

func (c *ChannelGuard) scanCheckTail() (bool, int) {
	if c.detector != nil {
		return c.detector.CheckTail()
	}
	if c.postEnd != nil {
		return c.postEnd.CheckTail()
	}
	return false, 0
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

	if len(delta) > 0 {
		// 关闭后由轻量尾部检测器继续扫描，绝不因 EndChannel 跳过周期检测。
		if hit, _ := c.scanPush(delta); hit {
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
		// 计算 snapshot 的抗碰撞摘要比对 baseline 摘要。
		// 使用 SHA-256 而非 64-bit FNV：攻击者无法以可接受成本构造同摘要异内容快照。
		// 摘要计算仍受 2*limit 扫描预算与 3 次终态许可约束（与旧实现一致，不增加全文处理量级）。
		currentDigest := sha256.Sum256(snapshot)
		if currentDigest == c.terminalBaselineHash {
			// 摘要完全一致，纯终态确认快照零计费放行，跳过步骤 7 重复扫描
			return growth, 0, true, nil
		}
		// 摘要不一致（变体篡改），作废该类型许可，全额计费
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

	// 终态首帧建立 baseline 摘要：仅白名单终态帧类型允许在首帧建立 baseline (Spec §5.3)。
	// 建立 baseline 与检测器是否仍存活无关：关闭后的白名单终态快照同样需要基线。
	shouldSetBaseline := !c.hasBaselineHash && allowedTerminalTypes[terminalStage]

	if cost > 0 {
		if shouldSetBaseline {
			c.terminalBaselineHash = sha256.Sum256(snapshot)
			c.hasBaselineHash = true
		}
		// 整帧快照独立扫描（关闭后由轻量尾部检测器继续执行，不再跳过）。
		c.resetScanners()
		if hit, _ := c.scanPush(snapshot); hit {
			return ErrRepetitionDetected
		}
		if hit, _ := c.scanCheckTail(); hit {
			return ErrRepetitionDetected
		}
	}

	return nil
}

func (c *ChannelGuard) CheckTail() error {
	if hit, _ := c.scanCheckTail(); hit {
		return ErrRepetitionDetected
	}
	return nil
}
