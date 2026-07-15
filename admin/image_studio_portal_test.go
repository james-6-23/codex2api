package admin

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strconv"
	"testing"

	"github.com/codex2api/auth"
	"github.com/codex2api/cache"
	"github.com/codex2api/database"
	"github.com/codex2api/proxy"
	"github.com/gin-gonic/gin"
)

func TestPortalImageJobIsolationAndAuth(t *testing.T) {
	gin.SetMode(gin.TestMode)

	db := newTestAdminDB(t)
	tc := cache.NewMemory(1)
	defer tc.Close()
	store := auth.NewStore(db, tc, nil)
	handler := NewHandler(store, db, tc, nil, "admin-secret")
	imageProxy := proxy.NewHandler(store, db, nil, nil)
	imageProxy.SetRuntimeCache(tc)
	handler.imageProxy = imageProxy

	// Enable portal page (default true) and register routes.
	ctx := context.Background()
	settings, err := db.GetSystemSettings(ctx)
	if err != nil {
		t.Fatalf("GetSystemSettings: %v", err)
	}
	if settings == nil {
		settings = &database.SystemSettings{PublicImageStudioPageEnabled: true}
	}
	settings.PublicImageStudioPageEnabled = true
	if err := db.UpdateSystemSettings(ctx, settings); err != nil {
		t.Fatalf("UpdateSystemSettings: %v", err)
	}

	router := gin.New()
	handler.RegisterRoutes(router)

	ownerID, err := db.InsertAPIKey(ctx, "owner", "sk-portal-owner")
	if err != nil {
		t.Fatalf("InsertAPIKey owner: %v", err)
	}
	if _, err := db.InsertAPIKey(ctx, "other", "sk-portal-other"); err != nil {
		t.Fatalf("InsertAPIKey other: %v", err)
	}

	jobID, err := db.InsertImageGenerationJob(ctx, database.ImageGenerationJobInput{
		Prompt:       "owner job",
		ParamsJSON:   `{}`,
		APIKeyID:     ownerID,
		APIKeyName:   "owner",
		APIKeyMasked: "sk-p...ner",
	})
	if err != nil {
		t.Fatalf("InsertImageGenerationJob: %v", err)
	}

	// Missing API key → 401
	unauth := httptest.NewRecorder()
	router.ServeHTTP(unauth, httptest.NewRequest(http.MethodGet, "/api/image-studio/jobs", nil))
	if unauth.Code != http.StatusUnauthorized {
		t.Fatalf("unauth status = %d, want 401 body=%s", unauth.Code, unauth.Body.String())
	}

	// Owner can list own jobs
	listReq := httptest.NewRequest(http.MethodGet, "/api/image-studio/jobs", nil)
	listReq.Header.Set("Authorization", "Bearer sk-portal-owner")
	listRec := httptest.NewRecorder()
	router.ServeHTTP(listRec, listReq)
	if listRec.Code != http.StatusOK {
		t.Fatalf("list status = %d, want 200 body=%s", listRec.Code, listRec.Body.String())
	}

	// Other key cannot read owner job
	otherGet := httptest.NewRequest(http.MethodGet, "/api/image-studio/jobs/"+strconv.FormatInt(jobID, 10), nil)
	otherGet.Header.Set("Authorization", "Bearer sk-portal-other")
	otherRec := httptest.NewRecorder()
	router.ServeHTTP(otherRec, otherGet)
	if otherRec.Code != http.StatusNotFound {
		t.Fatalf("other get status = %d, want 404 body=%s", otherRec.Code, otherRec.Body.String())
	}

	// Owner can get own job
	ownerGet := httptest.NewRequest(http.MethodGet, "/api/image-studio/jobs/"+strconv.FormatInt(jobID, 10), nil)
	ownerGet.Header.Set("Authorization", "Bearer sk-portal-owner")
	ownerRec := httptest.NewRecorder()
	router.ServeHTTP(ownerRec, ownerGet)
	if ownerRec.Code != http.StatusOK {
		t.Fatalf("owner get status = %d, want 200 body=%s", ownerRec.Code, ownerRec.Body.String())
	}

	// Disable portal → 404
	settings.PublicImageStudioPageEnabled = false
	if err := db.UpdateSystemSettings(ctx, settings); err != nil {
		t.Fatalf("disable portal: %v", err)
	}
	disabledReq := httptest.NewRequest(http.MethodGet, "/api/image-studio/jobs", nil)
	disabledReq.Header.Set("Authorization", "Bearer sk-portal-owner")
	disabledRec := httptest.NewRecorder()
	router.ServeHTTP(disabledRec, disabledReq)
	if disabledRec.Code != http.StatusNotFound {
		t.Fatalf("disabled status = %d, want 404 body=%s", disabledRec.Code, disabledRec.Body.String())
	}
}
