package database

import (
	"context"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"
	"unsafe"
)

func TestPromptFilterAuditQueueReservesHighPriorityCapacity(t *testing.T) {
	queue := newPromptFilterAuditQueue(&DB{})
	for index := 0; index < promptFilterAuditLowCapacity; index++ {
		if !queue.enqueue(PromptFilterLogInput{Source: "low"}, PromptFilterLogPriorityLow) {
			t.Fatalf("low priority enqueue %d failed before its dedicated capacity", index)
		}
	}
	if queue.enqueue(PromptFilterLogInput{Source: "overflow"}, PromptFilterLogPriorityLow) {
		t.Fatal("low priority queue accepted an item beyond its bounded capacity")
	}
	if !queue.enqueue(PromptFilterLogInput{Source: "block", Action: "block"}, PromptFilterLogPriorityHigh) {
		t.Fatal("low priority saturation consumed reserved high priority capacity")
	}
	if got := queue.droppedLow.Load(); got != 1 {
		t.Fatalf("dropped low = %d, want 1", got)
	}
}

func TestEnqueuePromptFilterLogPersistsAsynchronously(t *testing.T) {
	db, err := New("sqlite", filepath.Join(t.TempDir(), "codex2api.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()

	input := &PromptFilterLogInput{Source: "local_filter", Endpoint: "/v1/responses", Action: "block", TextPreview: "bounded prompt"}
	if !db.EnqueuePromptFilterLog(input, PromptFilterLogPriorityHigh) {
		t.Fatal("audit enqueue failed")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	if !db.WaitPromptFilterAuditIdle(ctx) {
		t.Fatal("audit queue did not become idle")
	}
	logs, err := db.ListPromptFilterLogs(ctx, 10)
	if err != nil {
		t.Fatal(err)
	}
	if len(logs) != 1 || logs[0].Endpoint != "/v1/responses" || logs[0].TextPreview != "bounded prompt" {
		t.Fatalf("persisted audit logs = %+v", logs)
	}
}

func TestPromptFilterAuditQueueDropsOversizedEvidence(t *testing.T) {
	queue := newPromptFilterAuditQueue(&DB{})
	input := PromptFilterLogInput{FullText: strings.Repeat("x", promptFilterAuditMaxJobBytes+1)}
	if queue.enqueue(input, PromptFilterLogPriorityHigh) {
		t.Fatal("oversized audit evidence entered the queue")
	}
	if got := queue.droppedHigh.Load(); got != 1 {
		t.Fatalf("dropped high = %d, want 1", got)
	}
}

func TestPromptFilterAuditQueueReservesHighPriorityBytes(t *testing.T) {
	queue := newPromptFilterAuditQueue(&DB{})
	input := PromptFilterLogInput{FullText: strings.Repeat("x", 128*1024)}
	for queue.enqueue(input, PromptFilterLogPriorityLow) {
	}
	if got := queue.retainedLow.Load(); got > promptFilterAuditMaxLowBytes {
		t.Fatalf("retained low-priority bytes = %d, max = %d", got, promptFilterAuditMaxLowBytes)
	}
	if !queue.enqueue(PromptFilterLogInput{Source: "block", FullText: strings.Repeat("y", 128*1024)}, PromptFilterLogPriorityHigh) {
		t.Fatal("low-priority byte saturation consumed the high-priority byte reserve")
	}
}

func TestPromptFilterAuditQueueOwnsQueuedStrings(t *testing.T) {
	queue := newPromptFilterAuditQueue(&DB{})
	backing := strings.Repeat("x", 4*1024*1024)
	preview := backing[:32]
	if !queue.enqueue(PromptFilterLogInput{TextPreview: preview}, PromptFilterLogPriorityLow) {
		t.Fatal("enqueue failed")
	}
	job := <-queue.low
	if unsafe.StringData(job.input.TextPreview) == unsafe.StringData(preview) {
		t.Fatal("queued preview retained the caller's backing allocation")
	}
}

func TestPromptPolicyIncidentQueueOwnsStringsAndRejectsOversizedJobs(t *testing.T) {
	queue := newPromptFilterAuditQueue(&DB{})
	backing := strings.Repeat("i", 4*1024*1024)
	preview := backing[:64]
	incident, candidate, evidence := promptPolicyTestInputs("incident-owned")
	incident.PromptPreview = preview
	incident.PromptText = preview
	candidate.SamplePreview = preview
	evidence.SamplePreview = preview
	if !queue.enqueueIncident(incident, candidate, evidence) {
		t.Fatal("incident enqueue failed")
	}
	job := <-queue.high
	queue.pending.Add(-1)
	queue.releaseBytes(PromptFilterLogPriorityHigh, job.bytes)
	if unsafe.StringData(job.incident.PromptPreview) == unsafe.StringData(preview) ||
		unsafe.StringData(job.incident.PromptText) == unsafe.StringData(preview) ||
		unsafe.StringData(job.candidate.SamplePreview) == unsafe.StringData(preview) ||
		unsafe.StringData(job.candidateEvidence.SamplePreview) == unsafe.StringData(preview) {
		t.Fatal("queued incident retained the caller's backing allocation")
	}

	incident.IncidentID = "incident-oversized"
	incident.PromptText = strings.Repeat("x", promptFilterAuditMaxJobBytes+1)
	if queue.enqueueIncident(incident, candidate, evidence) {
		t.Fatal("oversized incident entered the queue")
	}
	if got := queue.droppedHigh.Load(); got != 1 {
		t.Fatalf("dropped high = %d, want 1", got)
	}
}

func TestEnqueuePromptPolicyIncidentPersistsCompositeAndDrains(t *testing.T) {
	db, err := New("sqlite", filepath.Join(t.TempDir(), "codex2api.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()

	incident, candidate, evidence := promptPolicyTestInputs("incident-queued")
	if !db.EnqueuePromptPolicyIncident(&incident, &candidate, &evidence) {
		t.Fatal("incident audit enqueue failed")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	if !db.WaitPromptFilterAuditIdle(ctx) {
		t.Fatal("incident audit queue did not become idle")
	}
	got, err := db.GetPromptPolicyIncident(ctx, incident.IncidentID)
	if err != nil {
		t.Fatalf("GetPromptPolicyIncident: %v", err)
	}
	if got.CandidateID == 0 || got.CandidateEvidenceID == 0 {
		t.Fatalf("composite associations were not persisted: %#v", got)
	}
	evidenceItems, err := db.ListPromptRuleCandidateEvidence(ctx, got.CandidateID, 10)
	if err != nil || len(evidenceItems) != 1 || evidenceItems[0].PromptPolicyIncidentID != incident.IncidentID {
		t.Fatalf("incident evidence items=%#v err=%v", evidenceItems, err)
	}
}

func TestPromptPolicyIncidentQueueRejectsAfterClose(t *testing.T) {
	queue := newPromptFilterAuditQueue(&DB{})
	queue.closed.Store(true)
	incident, candidate, evidence := promptPolicyTestInputs("incident-closed")
	if queue.enqueueIncident(incident, candidate, evidence) {
		t.Fatal("closed queue accepted an incident")
	}
	if got := queue.droppedHigh.Load(); got != 1 {
		t.Fatalf("dropped high = %d, want 1", got)
	}
}

func TestPromptRuleCandidateQueueSaturationIsNonBlockingAndOwnsStrings(t *testing.T) {
	queue := newPromptFilterAuditQueue(&DB{})
	backing := strings.Repeat("z", 4*1024*1024)
	preview := backing[:64]
	candidate := PromptRuleCandidateInput{Fingerprint: strings.Repeat("a", 64), Kind: PromptRuleCandidateKindEvidence, SamplePreview: preview}
	evidence := PromptRuleCandidateEvidenceInput{SourceKind: PromptRuleCandidateSourceUpstreamCyberPolicy, SourceRefHash: strings.Repeat("b", 64), SamplePreview: preview}
	if !queue.enqueueCandidate(candidate, evidence, PromptFilterLogPriorityHigh) {
		t.Fatal("candidate enqueue failed")
	}
	job := <-queue.high
	queue.pending.Add(-1)
	queue.releaseBytes(PromptFilterLogPriorityHigh, job.bytes)
	if unsafe.StringData(job.candidate.SamplePreview) == unsafe.StringData(preview) || unsafe.StringData(job.candidateEvidence.SamplePreview) == unsafe.StringData(preview) {
		t.Fatal("queued candidate retained the caller's backing allocation")
	}
	for index := 0; index < promptFilterAuditHighCapacity; index++ {
		if !queue.enqueueCandidate(candidate, evidence, PromptFilterLogPriorityHigh) {
			t.Fatalf("candidate enqueue %d failed before dedicated capacity", index)
		}
	}
	started := time.Now()
	if queue.enqueueCandidate(candidate, evidence, PromptFilterLogPriorityHigh) {
		t.Fatal("saturated candidate queue accepted another job")
	}
	if elapsed := time.Since(started); elapsed > 50*time.Millisecond {
		t.Fatalf("saturated enqueue blocked for %s", elapsed)
	}
	if queue.droppedHigh.Load() != 1 {
		t.Fatalf("dropped high=%d, want 1", queue.droppedHigh.Load())
	}
}

func TestPromptFilterAuditQueueCloseRejectsConcurrentEnqueue(t *testing.T) {
	db, err := New("sqlite", filepath.Join(t.TempDir(), "codex2api.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	queue := db.promptFilterAudit

	var stop atomic.Bool
	var workers sync.WaitGroup
	for range 8 {
		workers.Add(1)
		go func() {
			defer workers.Done()
			for !stop.Load() {
				_ = queue.enqueue(PromptFilterLogInput{Source: "racing"}, PromptFilterLogPriorityLow)
			}
		}()
	}
	time.Sleep(10 * time.Millisecond)
	queue.close(2 * time.Second)
	stop.Store(true)
	workers.Wait()
	if queue.enqueue(PromptFilterLogInput{Source: "closed"}, PromptFilterLogPriorityHigh) {
		t.Fatal("closed queue accepted an audit record")
	}
	// close() 的排空预算是有界的(timeout + cancel + 250ms 收尾),按设计允许先于
	// worker 排空完成返回(见 close 的注释)。-race 的 2 核 CI runner 上 8 个
	// goroutine 压出的积压在预算内排不完,close 先行返回,此时 pending 还没归零
	// 属于正常中间态。清空断言只在 worker 全部退出后才成立,这里显式等 done,
	// 避免把"CI 慢"误判成"队列泄漏"(2026-08-26 main CI 曾因此假红)。
	select {
	case <-queue.done:
	case <-time.After(30 * time.Second):
		t.Fatal("audit workers did not finish draining after close")
	}
	if queue.pending.Load() != 0 || queue.retainedHigh.Load() != 0 || queue.retainedLow.Load() != 0 {
		t.Fatalf("closed queue retained work: pending=%d high_bytes=%d low_bytes=%d", queue.pending.Load(), queue.retainedHigh.Load(), queue.retainedLow.Load())
	}
	if queue.droppedHigh.Load() == 0 {
		t.Fatal("closed-queue rejection was not counted as a high-priority drop")
	}
}
