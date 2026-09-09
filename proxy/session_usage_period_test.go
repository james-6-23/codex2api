package proxy

import (
	"testing"
	"time"

	"github.com/codex2api/auth"
	"github.com/codex2api/database"
)

func TestWindowUsagePeriodIsCapturedPerSelectionAndClearedForNextFrame(test *testing.T) {
	store := auth.NewStore(nil, nil, &database.SystemSettings{})
	test.Cleanup(store.Stop)
	account := &auth.Account{DBID: 9, AccessToken: "test", Status: auth.StatusReady, SessionCapacityEnabled: true, SessionCapacityMax: 5, SessionCapacityIdleTTLSeconds: 3600}
	store.AddAccount(account)
	handler := &Handler{store: store}
	context := promptSessionLimitTestContext("root-one")
	body := []byte(`{"model":"gpt-6-astra"}`)
	var previousID string
	for _, root := range []string{"root-one", "root-two"} {
		context.Request.Header.Set("Session-Id", root)
		if !store.AdmitAccountSession(account, root, time.Now()) {
			test.Fatal("admission failed")
		}
		if _, exceeded := handler.checkPromptSessionCreationLimitForSelectedAccountAdmission(context, body, account, root, 0); exceeded {
			test.Fatal("unexpected user limit")
		}
		input := &database.UsageLogInput{AccountID: account.ID()}
		handler.populateAccountSessionObservation(context, input)
		expected := store.AccountSessionUsagePeriod(account.ID(), root, time.Now())
		if input.SessionUsagePeriodID == "" || input.SessionUsagePeriodID == previousID || input.SessionUsagePeriodID != expected.ID || !input.SessionUsageStartedAt.Equal(expected.StartedAt) {
			test.Fatalf("input=%+v expected=%+v previous=%s", input, expected, previousID)
		}
		previousID = input.SessionUsagePeriodID
	}
	handler.checkPromptSessionCreationLimitForSelectedAccountAdmission(context, body, nil, "", 0)
	input := &database.UsageLogInput{AccountID: account.ID()}
	handler.populateAccountSessionObservation(context, input)
	if input.SessionUsagePeriodID != "" {
		test.Fatalf("previous frame leaked period: %+v", input)
	}
}
