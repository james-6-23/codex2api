package admin

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/codex2api/proxy"
	"github.com/gin-gonic/gin"
)

func TestGetCodexUserAgentCatalogListsKinds(t *testing.T) {
	gin.SetMode(gin.TestMode)
	handler := &Handler{}
	recorder := httptest.NewRecorder()
	ctx, _ := gin.CreateTestContext(recorder)
	ctx.Request = httptest.NewRequest(http.MethodGet, "/api/admin/settings/codex-user-agent/catalog", nil)

	handler.GetCodexUserAgentCatalog(ctx)

	if recorder.Code != http.StatusOK {
		t.Fatalf("status = %d, body %s", recorder.Code, recorder.Body.String())
	}
	var view proxy.CodexUserAgentCatalogView
	if err := json.Unmarshal(recorder.Body.Bytes(), &view); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if len(view.Kinds) != 4 || view.Kinds[1].Kind != "codex-desktop" || view.Kinds[1].ClientName != "Codex Desktop" {
		t.Fatalf("unexpected kinds: %+v", view.Kinds)
	}
	if view.Kinds[1].AppFollowsCLI || len(view.Kinds[1].VersionPairs) == 0 || view.Kinds[0].AppFollowsCLI == false {
		t.Fatalf("catalog shape wrong: desktop=%+v tui=%+v", view.Kinds[1], view.Kinds[0])
	}
	if view.DefaultPoolMix["codex-desktop"] == 0 {
		t.Fatalf("default pool mix missing: %+v", view.DefaultPoolMix)
	}
}

func TestPreviewCodexUserAgentUsesFormValues(t *testing.T) {
	gin.SetMode(gin.TestMode)
	handler := &Handler{}
	body := `{"config":"{\"client_kind\":\"codex-desktop\",\"client_version\":\"0.152.0\"}","client_compat_mode":"auto","codex_min_cli_version":"0.153.0"}`
	recorder := httptest.NewRecorder()
	ctx, _ := gin.CreateTestContext(recorder)
	ctx.Request = httptest.NewRequest(http.MethodPost, "/api/admin/settings/codex-user-agent/preview", strings.NewReader(body))
	ctx.Request.Header.Set("Content-Type", "application/json")

	handler.PreviewCodexUserAgent(ctx)

	if recorder.Code != http.StatusOK {
		t.Fatalf("status = %d, body %s", recorder.Code, recorder.Body.String())
	}
	var preview proxy.CodexUserAgentPreview
	if err := json.Unmarshal(recorder.Body.Bytes(), &preview); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if preview.Persona == nil || preview.Persona.Version != "0.152.0" {
		t.Fatalf("manual CLI override should retain 0.152.0, got %+v", preview.Persona)
	}
	if !strings.HasSuffix(preview.Persona.UserAgent, "(Codex Desktop; 26.930.51102)") || preview.Persona.Originator != "Codex Desktop" {
		t.Fatalf("persona = %+v", preview.Persona)
	}

	recorder = httptest.NewRecorder()
	ctx, _ = gin.CreateTestContext(recorder)
	ctx.Request = httptest.NewRequest(http.MethodPost, "/api/admin/settings/codex-user-agent/preview", strings.NewReader(`{"config":"{\"client_kind\":\"nope\"}"}`))
	ctx.Request.Header.Set("Content-Type", "application/json")
	handler.PreviewCodexUserAgent(ctx)
	if recorder.Code != http.StatusBadRequest {
		t.Fatalf("invalid config status = %d, want 400", recorder.Code)
	}
}
