package database

import (
	"encoding/json"
	"strings"
	"testing"

	"github.com/stretchr/testify/require"
)

func TestServiceErrorQueueKeepsWindowDecisionSnapshot(test *testing.T) {
	db := &DB{}
	db.serviceErrors = newServiceErrorQueue(db)
	defer db.serviceErrors.cancel()
	diagnostic := &WindowControlDiagnostic{
		RootHash: strings.Repeat("r", 200), Decision: "owner_admission_rejected",
		Grant:   &WindowGrantDiagnostic{State: "expired"},
		Account: &AccountSessionAdmissionDiagnostic{Reason: "session_capacity_full", SlotState: "missing", TotalUsed: 8},
	}
	require.True(test, db.EnqueueServiceError(ServiceErrorEvent{ID: "window-decision", StatusCode: 400, WindowControl: diagnostic}))
	diagnostic.Decision = "changed"
	diagnostic.Grant.State = "changed"
	diagnostic.Account.TotalUsed = 0
	job := <-db.serviceErrors.jobs
	require.Equal(test, "owner_admission_rejected", job.event.WindowControl.Decision)
	require.Equal(test, "expired", job.event.WindowControl.Grant.State)
	require.EqualValues(test, 8, job.event.WindowControl.Account.TotalUsed)
	require.Len(test, job.event.WindowControl.RootHash, 80)
	var persisted ServiceErrorEvent
	require.NoError(test, json.Unmarshal([]byte(job.payload), &persisted))
	require.Equal(test, job.event.WindowControl, persisted.WindowControl)
}
