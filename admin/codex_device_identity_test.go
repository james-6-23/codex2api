package admin

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/codex2api/auth"
	"github.com/codex2api/database"
	"github.com/gin-gonic/gin"
)

func TestCodexDeviceIdentityImportAndAdminDetailAgree(t *testing.T) {
	gin.SetMode(gin.TestMode)
	db := newTestAdminDB(t)
	store := auth.NewStore(db, nil, nil)
	defer store.Stop()
	handler := &Handler{db: db, store: store}
	seed := tokenCredentialSeed{refreshToken: "rt-without-upstream-uuid"}
	credentials := handler.newCodexAccountCredentials(&seed)
	accountID, err := db.InsertAccountWithCredentials(context.Background(), "device", credentials, "")
	if err != nil {
		t.Fatal(err)
	}
	account := handler.newCodexAccountFromSeed(accountID, "", seed)
	want := account.EffectiveCodexInstallationID()
	if want == "" || want != credentials[database.CodexInstallationIDCredentialKey] || account.AccountID != "" {
		t.Fatal("imported runtime identity must match persisted identity before UUID lookup")
	}
	if _, exists := tokenCredentialMap(seed)[database.CodexInstallationIDCredentialKey]; exists {
		t.Fatal("credential refresh must not overwrite the saved installation ID")
	}
	store.AddAccount(account)
	for _, custom := range []string{"", "custom-device-id", ""} {
		if err := db.UpdateCredentials(context.Background(), accountID, map[string]interface{}{"custom_headers": map[string]string{"X-Codex-Installation-Id": custom}}); err != nil {
			t.Fatal(err)
		}
		recorder := httptest.NewRecorder()
		ginContext, _ := gin.CreateTestContext(recorder)
		ginContext.Params = gin.Params{{Key: "id", Value: fmt.Sprint(accountID)}}
		ginContext.Request = httptest.NewRequest(http.MethodGet, fmt.Sprintf("/api/admin/accounts/%d", accountID), nil)
		handler.GetAccount(ginContext)
		var response accountResponse
		if recorder.Code != http.StatusOK || json.Unmarshal(recorder.Body.Bytes(), &response) != nil {
			t.Fatalf("detail response: %d %s", recorder.Code, recorder.Body.String())
		}
		expected := want
		if custom != "" {
			expected = custom
		}
		if response.CodexInstallationID != expected || response.CodexFingerprintMode != auth.CodexFingerprintModeOff {
			t.Fatalf("disabled-mode detail identity = %q, want %q", response.CodexInstallationID, expected)
		}
	}
}
