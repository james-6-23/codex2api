package auth

import (
	"sync/atomic"
	"testing"
	"time"
)

func TestIntelligencePauseFencesBusinessButAllowsBoundedProbe(t *testing.T) {
	s := NewStore(nil, nil, nil)
	s.SetMaxConcurrency(1)
	a := &Account{DBID: 1, AccessToken: "test", Status: StatusReady, HealthTier: HealthTierHealthy}
	s.AddAccount(a)
	atomic.StoreInt32(&a.IntelligencePaused, 1)
	if !accountDispatchBlocked(a) || a.ModelCatalogEligible() {
		t.Fatal("detection pause must block business/catalog")
	}
	_, _, _, _, available := a.fastSchedulerSnapshot(4, time.Now())
	if available {
		t.Fatal("fast scheduler ignored detection pause")
	}
	if !s.TryAcquireIntelligenceCheck(a) {
		t.Fatal("detection must continue during its own pause")
	}
	if s.TryAcquireIntelligenceCheck(a) {
		t.Fatal("hard default capacity exceeded")
	}
	s.Release(a)
	atomic.StoreInt32(&a.DispatchPaused, 1)
	atomic.StoreInt32(&a.IntelligencePaused, 0)
	if !accountDispatchBlocked(a) {
		t.Fatal("clearing quality pause must not enable manually disabled account")
	}
}
