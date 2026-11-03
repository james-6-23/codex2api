package outputguard

import (
	"bytes"
	"crypto/sha256"
	"errors"
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
	// 基准摘要绝不能被篡改快照覆盖，且必须是抗碰撞的 SHA-256（32B）而非 64-bit FNV
	wantDigest := sha256.Sum256(data1)
	if cg.terminalBaselineHash != wantDigest {
		t.Fatalf("expected baseline sha256 digest to stay unchanged")
	}
	if cg.terminalBaselineHash == sha256.Sum256(data2) {
		t.Fatalf("expected tampered snapshot to have a different sha256 digest")
	}
}

// B2 回归：终态等价比对使用抗碰撞 SHA-256 摘要。
// 旧实现使用 64-bit FNV-1a，攻击者可用碰撞让"改动过的快照"零成本通过；
// 这里直接验证 SHA-256 路径的选择性：同长度单字节篡改的摘要必然不同且被全额计费。
func TestChannelGuardTerminalBaselineDigestIsCollisionResistant(t *testing.T) {
	cg := NewChannelGuard("c_digest", KindToolArguments, 262144, 4096)
	data1 := bytes.Repeat([]byte("terminal-snapshot-"), 64)
	growth, cost, _, err := cg.CheckSnapshot(data1, "arguments.done")
	if err != nil {
		t.Fatalf("first snapshot CheckSnapshot error: %v", err)
	}
	if err := cg.CommitSnapshot(data1, growth, cost, "arguments.done"); err != nil {
		t.Fatalf("first snapshot CommitSnapshot error: %v", err)
	}

	if !cg.hasBaselineHash {
		t.Fatalf("expected baseline digest created")
	}
	if len(cg.terminalBaselineHash) != sha256.Size {
		t.Fatalf("baseline digest must be %d bytes (sha256), got %d", sha256.Size, len(cg.terminalBaselineHash))
	}
	if cg.terminalBaselineHash != sha256.Sum256(data1) {
		t.Fatalf("baseline digest must equal sha256(snapshot)")
	}

	variant := bytes.Clone(data1)
	variant[len(variant)-1] ^= 0x01
	if sha256.Sum256(variant) == cg.terminalBaselineHash {
		t.Fatalf("sha256 digest collision on single-byte mutation is unexpected")
	}

	growth, cost, skip, err := cg.CheckSnapshot(variant, "item.done")
	if err != nil {
		t.Fatalf("variant CheckSnapshot error: %v", err)
	}
	if skip {
		t.Fatalf("mutated variant must not be treated as equivalent terminal snapshot")
	}
	if cost != int64(len(variant)) {
		t.Fatalf("mutated variant must be fully charged, want %d got %d", len(variant), cost)
	}
}

// B1 回归：channel 结束（ReleaseWindow）后迟到的 append 仍必须接受周期检测。
// 对应 Anthropic 在 content_block_stop 之后仍下发 content_block_delta 的场景。
func TestChannelGuardPostEndAppendStillScans(t *testing.T) {
	cg := NewChannelGuard("c_post_append", KindText, 1<<20, 4096)
	prefix := []byte("non-repeating-prefix-")
	growth, cost, err := cg.CheckAppend(prefix)
	if err != nil {
		t.Fatalf("prefix CheckAppend error: %v", err)
	}
	if err := cg.CommitAppend(prefix, growth, cost); err != nil {
		t.Fatalf("prefix CommitAppend error: %v", err)
	}

	cg.ReleaseWindow()
	if cg.isClosed != true || cg.detector != nil || cg.postEnd == nil {
		t.Fatalf("ReleaseWindow must close channel and switch to bounded post-end detector")
	}

	periodic := bytes.Repeat([]byte("0123456789"), 1024) // 10240 字节，周期 10
	var scanErr error
	for off := 0; off < len(periodic); off += 1024 {
		end := off + 1024
		if end > len(periodic) {
			end = len(periodic)
		}
		chunk := periodic[off:end]
		growth, cost, err := cg.CheckAppend(chunk)
		if err != nil {
			t.Fatalf("post-end CheckAppend error: %v", err)
		}
		if err := cg.CommitAppend(chunk, growth, cost); err != nil {
			scanErr = err
			break
		}
	}
	if !errors.Is(scanErr, ErrRepetitionDetected) {
		t.Fatalf("post-end append must still trigger repetition detection, got %v", scanErr)
	}
}

// B1 回归：channel 结束后的 snapshot 仍必须接受周期检测。
func TestChannelGuardPostEndSnapshotStillScans(t *testing.T) {
	cg := NewChannelGuard("c_post_snapshot", KindText, 1<<20, 4096)
	cg.ReleaseWindow()

	snapshot := bytes.Repeat([]byte("abcdef"), 1500) // 9000 字节，周期 6
	growth, cost, skip, err := cg.CheckSnapshot(snapshot, "unknown_or_intermediate_type")
	if err != nil {
		t.Fatalf("post-end CheckSnapshot error: %v", err)
	}
	if skip {
		t.Fatalf("non-terminal snapshot must not be treated as equivalent")
	}
	if err := cg.CommitSnapshot(snapshot, growth, cost, "unknown_or_intermediate_type"); !errors.Is(err, ErrRepetitionDetected) {
		t.Fatalf("post-end snapshot must still trigger repetition detection, got %v", err)
	}
}

// B1 回归：结束后扫描仍受 2*limit 扫描预算约束，不能借关闭通道绕过预算。
func TestChannelGuardPostEndScanBudgetStillEnforced(t *testing.T) {
	cg := NewChannelGuard("c_post_budget", KindText, 4096, 4096)
	if cg.RemainingScanBudget() != 8192 {
		t.Fatalf("want initial scan budget 8192, got %d", cg.RemainingScanBudget())
	}

	r := rand.New(rand.NewSource(424242))
	full := make([]byte, 4096)
	r.Read(full)
	growth, cost, err := cg.CheckAppend(full)
	if err != nil {
		t.Fatalf("pre-end CheckAppend error: %v", err)
	}
	if err := cg.CommitAppend(full, growth, cost); err != nil {
		t.Fatalf("pre-end CommitAppend error: %v", err)
	}
	if cg.RemainingScanBudget() != 4096 {
		t.Fatalf("pre-end append must consume 4096 budget, remaining=%d", cg.RemainingScanBudget())
	}

	cg.ReleaseWindow()

	// 结束后的快照扫描同样扣减预算（非重复数据不触发检测，但必须计费）。
	growth, cost, _, err = cg.CheckSnapshot(full, "unknown_or_intermediate_type")
	if err != nil {
		t.Fatalf("post-end CheckSnapshot error: %v", err)
	}
	if err := cg.CommitSnapshot(full, growth, cost, "unknown_or_intermediate_type"); err != nil {
		t.Fatalf("post-end CommitSnapshot error: %v", err)
	}
	if cg.RemainingScanBudget() != 0 {
		t.Fatalf("post-end snapshot must consume remaining budget, remaining=%d", cg.RemainingScanBudget())
	}

	// 预算耗尽后，结束后的快照不得再获得免费扫描。
	if _, _, _, err := cg.CheckSnapshot(full, "unknown_or_intermediate_type"); !errors.Is(err, ErrScanBudgetExhausted) {
		t.Fatalf("post-end scan must fail once budget exhausted, got %v", err)
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
