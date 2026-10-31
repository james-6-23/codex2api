package outputguard

import (
	"bytes"
	"math/rand"
	"testing"
)

func TestChannelGuardHighwaterGrowthAndScanBudget(t *testing.T) {
	// 工具 channel: maxBytes = 256 KiB (262144), scanBudget = 2 * 256 KiB = 512 KiB
	cg := NewChannelGuard("call_1", KindToolArguments, 262144, 4096)

	// Append 64 KiB of non-repeating data
	delta1 := make([]byte, 65536)
	r1 := rand.New(rand.NewSource(10001))
	r1.Read(delta1)

	growth, cost, err := cg.CheckAppend(delta1)
	if err != nil {
		t.Fatalf("CheckAppend delta1 error: %v", err)
	}
	if growth != 65536 || cost != 65536 {
		t.Fatalf("delta1: want growth=65536, cost=65536, got growth=%d, cost=%d", growth, cost)
	}
	if err := cg.CommitAppend(delta1, growth, cost); err != nil {
		t.Fatalf("CommitAppend delta1 error: %v", err)
	}

	if cg.Highwater() != 65536 {
		t.Fatalf("want highwater=65536, got %d", cg.Highwater())
	}
	if cg.RemainingScanBudget() != 524288-65536 {
		t.Fatalf("want remainingScanBudget=%d, got %d", 524288-65536, cg.RemainingScanBudget())
	}
}

// Spec §9.1 Case 8a: 耗尽扫描预算后变体快照超额拦截
func TestChannelGuardCase8a_ScanBudgetExhaustionAndVariantRejection(t *testing.T) {
	cg := NewChannelGuard("call_1", KindToolArguments, 262144, 4096)
	// 以 256 次 1 KiB delta 耗尽 256 KiB 扫描预算 (剩余 256 KiB)
	chunk := make([]byte, 1024)
	r2 := rand.New(rand.NewSource(20002))
	r2.Read(chunk)
	var fullParam []byte
	for i := 0; i < 256; i++ {
		fullParam = append(fullParam, chunk...)
		growth, cost, err := cg.CheckAppend(chunk)
		if err != nil {
			t.Fatalf("step %d append error: %v", i, err)
		}
		if err := cg.CommitAppend(chunk, growth, cost); err != nil {
			t.Fatalf("step %d commit append error: %v", i, err)
		}
	}
	if cg.RemainingScanBudget() != 262144 {
		t.Fatalf("remaining scan budget want 262144, got %d", cg.RemainingScanBudget())
	}

	// 首个 256 KiB 白名单终态快照到达 (arguments.done)，尚无 baseline hash，cost = 256 KiB
	growth, cost, skipRepetition, err := cg.CheckSnapshot(fullParam, "arguments.done")
	if err != nil {
		t.Fatalf("arguments.done snapshot error: %v", err)
	}
	if cost != 262144 || growth != 0 || skipRepetition {
		t.Fatalf("arguments.done: want cost=262144, growth=0, skipRepetition=false; got cost=%d, growth=%d, skip=%v", cost, growth, skipRepetition)
	}
	if err := cg.CommitSnapshot(fullParam, growth, cost, "arguments.done"); err != nil {
		t.Fatalf("arguments.done commit snapshot error: %v", err)
	}

	if cg.RemainingScanBudget() != 0 {
		t.Fatalf("remaining scan budget want 0, got %d", cg.RemainingScanBudget())
	}

	// 紧接着收到修改了 1 个字节的 256 KiB 变体快照 (item.done)
	variantParam := bytes.Clone(fullParam)
	variantParam[65536] ^= 0xFF

	_, _, _, err = cg.CheckSnapshot(variantParam, "item.done")
	if err == nil {
		t.Fatalf("expected variant snapshot to be rejected with scan budget exhaustion, got nil")
	}
	if err != ErrScanBudgetExhausted {
		t.Fatalf("expected ErrScanBudgetExhausted, got %v", err)
	}
}

// Spec §9.1 Case 8b: 合法终态链零计费放行与无资格拒绝
func TestChannelGuardCase8b_TerminalChainZeroCostAndExhaustion(t *testing.T) {
	cg := NewChannelGuard("call_1", KindToolArguments, 262144, 4096)
	fullParam := make([]byte, 262144)
	r3 := rand.New(rand.NewSource(30003))
	r3.Read(fullParam)
	// append 256 KiB
	growth, cost, _ := cg.CheckAppend(fullParam)
	_ = cg.CommitAppend(fullParam, growth, cost)

	// arguments.done 消耗最后 256 KiB 扫描预算并建立 baseline hash
	growth, cost, _, _ = cg.CheckSnapshot(fullParam, "arguments.done")
	_ = cg.CommitSnapshot(fullParam, growth, cost, "arguments.done")

	// item.done 具备资格且 Hash 完全一致 -> cost = 0, skipRepetition = true
	growth, cost, skipRepetition, err := cg.CheckSnapshot(fullParam, "item.done")
	if err != nil {
		t.Fatalf("item.done snapshot error: %v", err)
	}
	if cost != 0 || !skipRepetition {
		t.Fatalf("item.done: want cost=0, skipRepetition=true; got cost=%d, skip=%v", cost, skipRepetition)
	}
	_ = cg.CommitSnapshot(fullParam, growth, cost, "item.done")

	// response.completed 同样具备资格且 Hash 一致 -> cost = 0
	growth, cost, skipRepetition, err = cg.CheckSnapshot(fullParam, "response.completed")
	if err != nil {
		t.Fatalf("response.completed snapshot error: %v", err)
	}
	if cost != 0 || !skipRepetition {
		t.Fatalf("response.completed: want cost=0, skipRepetition=true")
	}
	_ = cg.CommitSnapshot(fullParam, growth, cost, "response.completed")

	// 重发同类型 response.completed -> 失去资格，走 cost = 256 KiB，因剩余扫描预算为 0 被拦截
	_, _, _, err = cg.CheckSnapshot(fullParam, "response.completed")
	if err != ErrScanBudgetExhausted {
		t.Fatalf("duplicate response.completed: want ErrScanBudgetExhausted, got %v", err)
	}
}

// Spec §9.1 Case 8c: 非白名单普通快照绝不建立 baseline
func TestChannelGuardCase8c_NonTerminalSnapshotCannotEstablishBaseline(t *testing.T) {
	cg := NewChannelGuard("c_text", KindText, 2097152, 4096)
	snap := []byte("intermediate full snapshot text")
	growth, cost, skip, err := cg.CheckSnapshot(snap, "unknown_or_intermediate_type")
	if err != nil {
		t.Fatalf("CheckSnapshot error: %v", err)
	}
	if skip || cost != int64(len(snap)) {
		t.Fatalf("want skip=false, cost=%d, got skip=%v, cost=%d", len(snap), skip, cost)
	}
	_ = cg.CommitSnapshot(snap, growth, cost, "unknown_or_intermediate_type")

	// 确认 hasBaselineHash 仍为 false，未建立 baseline
	if cg.hasBaselineHash {
		t.Fatalf("expected hasBaselineHash=false for non-terminal snapshot, got true")
	}
}

func TestChannelGuardHashMismatchDiscardsQuotaAndChargesFullCost(t *testing.T) {
	cg := NewChannelGuard("c_tool", KindToolArguments, 262144, 4096)
	data1 := bytes.Repeat([]byte("hello world "), 100) // 1200 字节
	growth, cost, _, _ := cg.CheckSnapshot(data1, "arguments.done")
	_ = cg.CommitSnapshot(data1, growth, cost, "arguments.done")

	if !cg.hasBaselineHash {
		t.Fatalf("expected baseline hash created")
	}

	// 紧接着发送篡改变体快照
	data2 := bytes.Repeat([]byte("corrupted data!"), 80) // 1200 字节
	growth, cost, skip, err := cg.CheckSnapshot(data2, "item.done")
	if err != nil {
		t.Fatalf("unexpected CheckSnapshot error: %v", err)
	}
	if skip {
		t.Fatalf("expected skip=false for hash mismatch")
	}
	if cost != int64(len(data2)) {
		t.Fatalf("expected full cost %d, got %d", len(data2), cost)
	}

	// CommitSnapshot 篡改帧后，该类型许可必须标记废弃
	_ = cg.CommitSnapshot(data2, growth, cost, "item.done")
	if !cg.consumedTerminalTypes["item.done"] {
		t.Fatalf("expected item.done marked as consumed/discarded")
	}
	// 基准 Hash 绝不能被篡改快照覆盖
	h := fnv64a(data1)
	if cg.terminalBaselineHash != h {
		t.Fatalf("expected baseline hash to stay unchanged")
	}
}

func TestChannelGuardTailCheck(t *testing.T) {
	cg := NewChannelGuard("c_text", KindText, 2097152, 4096)
	// 短文本放行
	_ = cg.CommitAppend([]byte("short text"), 10, 10)
	if err := cg.CheckTail(); err != nil {
		t.Fatalf("expected nil error for short text tail check, got %v", err)
	}

	// 长周期文本检出
	cg2 := NewChannelGuard("c_periodic", KindText, 2097152, 4096)
	pattern := []byte("0123456789")
	data := bytes.Repeat(pattern, 850) // 8500 字节
	// 避免在 append 触发检查点，直接写入 window
	cg2.detector.window = [8192]byte{}
	for i := 0; i < len(data); i++ {
		cg2.detector.window[i%8192] = data[i]
	}
	cg2.detector.count = int64(len(data))
	cg2.detector.head = len(data) % 8192

	if err := cg2.CheckTail(); err != ErrRepetitionDetected {
		t.Fatalf("expected ErrRepetitionDetected for periodic tail check, got %v", err)
	}
}
