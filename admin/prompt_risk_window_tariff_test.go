package admin

import (
	"encoding/json"
	"testing"
	"time"

	"github.com/codex2api/cache"
	"github.com/codex2api/database"
)

func TestPromptRiskSessionWindowsExposePersistedTariff(testContext *testing.T) {
	for _, scenario := range []struct {
		name       string
		expanded   bool
		multiplier float64
		wantRatio  float64
	}{
		{name: "expanded", expanded: true, multiplier: 1.5, wantRatio: 1.5},
		{name: "standard", multiplier: 1, wantRatio: 1},
		{name: "legacy", wantRatio: 1},
	} {
		testContext.Run(scenario.name, func(testContext *testing.T) {
			runtimeCache := cache.NewMemory(1)
			defer runtimeCache.Close()
			now := time.Now().UTC()
			state := cache.PromptSessionLimitState{
				Version:  2,
				Sessions: map[string]time.Time{"root": now.Add(time.Hour)},
				Details: map[string]cache.PromptSessionWindowDetail{
					"root": {Expanded: scenario.expanded, Multiplier: scenario.multiplier, GrantID: "private-grant"},
				},
			}
			raw, err := json.Marshal(state)
			if err != nil {
				testContext.Fatal(err)
			}
			err = runtimeCache.SetRuntime(testContext.Context(), cache.PromptSessionLimitRuntimeNamespace, cache.PromptSessionLimitSubject("newapi", "42"), raw, time.Hour)
			if err != nil {
				testContext.Fatal(err)
			}
			handler := &Handler{cache: runtimeCache}
			items := handler.promptRiskSessionWindows(testContext.Context(), &database.PromptRiskProfile{
				SubjectType: database.PromptRiskSubjectNewAPIUser, Platform: "newapi", NewAPIUserID: "42",
			}, now)
			payload, err := json.Marshal(items)
			if err != nil {
				testContext.Fatal(err)
			}
			var response []map[string]any
			if err := json.Unmarshal(payload, &response); err != nil {
				testContext.Fatal(err)
			}
			if len(response) != 1 || response[0]["expanded"] != scenario.expanded || response[0]["multiplier"] != scenario.wantRatio {
				testContext.Fatalf("unexpected window tariff: %s", payload)
			}
			if _, exposed := response[0]["grant_id"]; exposed {
				testContext.Fatal("profile must not expose admission grants")
			}
		})
	}
}
