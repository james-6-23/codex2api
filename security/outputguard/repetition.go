package outputguard

const (
	WindowSizeBytes     = 8192
	CheckpointStepBytes = 1024
	MaxRepetitionPeriod = 256
	MinRepetitionPeriod = 1
)

type RepetitionDetector struct {
	window                [WindowSizeBytes]byte
	linear                [WindowSizeBytes]byte
	pi                    [WindowSizeBytes]uint16
	count                 int64 // channel 累计写入总字节数
	head                  int   // ring buffer 写入游标 (0..8191)
	lastCheckedCheckpoint int64
}

func NewRepetitionDetector() *RepetitionDetector {
	return &RepetitionDetector{}
}

func (d *RepetitionDetector) Reset() {
	d.count = 0
	d.head = 0
	d.lastCheckedCheckpoint = 0
}

func (d *RepetitionDetector) writeChunk(chunk []byte) {
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

func (d *RepetitionDetector) bytesNeededToNextCheckpoint() int {
	if d.count < WindowSizeBytes {
		return int(WindowSizeBytes - d.count)
	}
	nextCheckpoint := d.lastCheckedCheckpoint + CheckpointStepBytes
	if nextCheckpoint <= d.count {
		nextCheckpoint = d.count - (d.count % CheckpointStepBytes) + CheckpointStepBytes
	}
	return int(nextCheckpoint - d.count)
}

func (d *RepetitionDetector) advanceCheckpoint() {
	d.lastCheckedCheckpoint = d.count - (d.count % CheckpointStepBytes)
}

func (d *RepetitionDetector) Push(data []byte) (hit bool, period int) {
	for len(data) > 0 {
		needed := d.bytesNeededToNextCheckpoint()

		if len(data) < needed {
			d.writeChunk(data)
			return false, 0
		}

		// 达到检查点
		d.writeChunk(data[:needed])
		data = data[needed:]
		d.advanceCheckpoint()
		if ok, p := d.checkCurrentWindow(); ok {
			return true, p
		}
	}
	return false, 0
}

// PushWithHash 一次遍历同时流水线计算 FNV-1a Hash 并执行滑动窗口重复检测 (Spec §5.3)
func (d *RepetitionDetector) PushWithHash(data []byte, initialHash uint64) (hit bool, period int, finalHash uint64) {
	h := initialHash
	for len(data) > 0 {
		needed := d.bytesNeededToNextCheckpoint()

		if len(data) < needed {
			for _, b := range data {
				h = (h ^ uint64(b)) * 1099511628211
			}
			d.writeChunk(data)
			return false, 0, h
		}

		chunk := data[:needed]
		for _, b := range chunk {
			h = (h ^ uint64(b)) * 1099511628211
		}
		d.writeChunk(chunk)
		data = data[needed:]
		d.advanceCheckpoint()
		if ok, p := d.checkCurrentWindow(); ok {
			return true, p, h
		}
	}
	return false, 0, h
}

func (d *RepetitionDetector) CheckTail() (hit bool, period int) {
	// Spec §5.2 规则 6: 如果 channel 累计总字节数 < 8192，不判定重复，直接放行
	if d.count < WindowSizeBytes {
		return false, 0
	}
	// 已满 8192 窗口但尾部未对齐 1024，提取最新的完整 8192 窗口执行一次求周期检查
	return d.checkCurrentWindow()
}

// checkCurrentWindow 提取当前 ring buffer 中的线性 8192 字节，使用前缀函数求最短周期 1 <= p <= 256
// 数学核实说明：有限字符串周期不要求 L % p == 0（例如事故周期 p=30, 8192 % 30 = 22 != 0）。
// 前缀函数的最大 border k = pi[L-1] 对应最小周期 p = L - k。
// 当且仅当 1 <= p <= 256 且满足全部 p <= i < 8192 的 window[i] == window[i-p] 时判定命中。
// 周期具有前缀遗传性：若前缀 [0..i] 的最小周期 (i+1)-pi[i] > 256，则整个窗口不可能 p <= 256，可立即早退。
func (d *RepetitionDetector) checkCurrentWindow() (bool, int) {
	if d.count < WindowSizeBytes {
		return false, 0
	}
	// 展开当前最新的 8192 字节连续视图：利用 copy 替代逐字节取模展开
	tailLen := WindowSizeBytes - d.head
	copy(d.linear[:tailLen], d.window[d.head:])
	copy(d.linear[tailLen:], d.window[:d.head])

	// 采用流式 KMP prefix function，并在最小周期超过 MaxRepetitionPeriod 时早退
	d.pi[0] = 0
	for i := 1; i < WindowSizeBytes; i++ {
		j := int(d.pi[i-1])
		for j > 0 && d.linear[i] != d.linear[j] {
			j = int(d.pi[j-1])
		}
		if d.linear[i] == d.linear[j] {
			j++
		}
		d.pi[i] = uint16(j)

		// 周期前缀遗传性早退：前缀 [0..i] 的最短周期为 (i+1) - pi[i]
		// 若前缀的最短周期已超过 MaxRepetitionPeriod (256)，则包含此前缀的整个 8192 窗口最短周期必 > 256
		if (i+1)-j > MaxRepetitionPeriod {
			return false, 0
		}
	}

	k := int(d.pi[WindowSizeBytes-1])
	p := WindowSizeBytes - k
	if p >= MinRepetitionPeriod && p <= MaxRepetitionPeriod {
		return true, p
	}

	return false, 0
}
