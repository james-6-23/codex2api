package database

import (
	"context"
	"testing"
	"time"
)

func TestPromptTraceQueueLeaseRetryAndRetention(t *testing.T) {
	db := newPromptRetentionTestDB(t)
	ctx := context.Background()
	now := time.Now().UTC()
	id, err := db.InsertPromptRequest(ctx, PromptRequestBody{
		PromptRequestRecord: PromptRequestRecord{CreatedAt: now.AddDate(0, 0, -45), Endpoint: "/v1/responses"},
	})
	if err != nil {
		t.Fatal(err)
	}
	const destination = "project"
	enqueue := func() {
		t.Helper()
		if err := db.EnqueuePromptRequestTraceBackfill(ctx, destination, []int64{id}, now); err != nil {
			t.Fatal(err)
		}
	}
	claim := func(at time.Time) *PromptRequestTraceJob {
		t.Helper()
		job, err := db.ClaimPromptRequestTrace(ctx, destination, at, at.Add(time.Second))
		if err != nil {
			t.Fatal(err)
		}
		return job
	}
	enqueue()
	enqueue()
	first := claim(now)
	if first == nil || first.DeliveryTry != 1 || claim(now) != nil {
		t.Fatal("expected one lease")
	}
	reclaimedAt := now.Add(2 * time.Second)
	reclaimed := claim(reclaimedAt)
	if reclaimed == nil || reclaimed.DeliveryTry != 2 || reclaimed.RequestID != id {
		t.Fatal("expired work was not reclaimed")
	}
	if err := db.FinishPromptRequestTrace(ctx, destination, first, "stale-attempt", reclaimedAt); err != nil {
		t.Fatal(err)
	}
	if got, err := db.GetPromptRequestTraceID(ctx, id, destination); err != nil || got != "" {
		t.Fatalf("stale worker completed a new lease: %q, %v", got, err)
	}
	nextAt := now.Add(time.Minute)
	if err := db.FinishPromptRequestTrace(ctx, destination, reclaimed, "", nextAt); err != nil || claim(reclaimedAt) != nil {
		t.Fatalf("failed job retried before backoff: %v", err)
	}
	retry := claim(nextAt)
	if retry == nil || retry.DeliveryTry != 3 {
		t.Fatal("expected retry at backoff deadline")
	}
	if err := db.FinishPromptRequestTrace(ctx, destination, retry, "delivered", nextAt); err != nil {
		t.Fatal(err)
	}
	enqueue()
	if claim(nextAt) != nil {
		t.Fatal("backfill enrolled an already exported request again")
	}
	if got, err := db.GetPromptRequestTraceID(ctx, id, destination); err != nil || got != "delivered" {
		t.Fatalf("completed trace missing: %q, %v", got, err)
	}
	if err := db.PurgeExpiredPromptRequests(ctx, now); err != nil {
		t.Fatal(err)
	}
	if got, err := db.GetPromptRequestTraceID(ctx, id, destination); err != nil || got != "" {
		t.Fatalf("trace metadata outlived local retention: %q, %v", got, err)
	}
}
