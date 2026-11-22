package admin

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"image"
	"image/png"
	"mime/multipart"
	"net"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/codex2api/database"
	"github.com/gin-gonic/gin"
)

func TestPromptRequestDetailsRequireAuthAndServeOriginalImages(t *testing.T) {
	handler, db := newTestAdminHandlerFixture(t)
	handler.ConfigurePromptRequestRecords(true)
	router := gin.New()
	handler.RegisterRoutes(router)
	ctx := context.Background()
	var rawPNG bytes.Buffer
	if err := png.Encode(&rawPNG, image.NewRGBA(image.Rect(0, 0, 4, 4))); err != nil {
		t.Fatal(err)
	}
	linkedImage := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "image/png")
		_, _ = w.Write(rawPNG.Bytes())
	}))
	t.Cleanup(linkedImage.Close)
	originalDial := dialPublicExternalInputImageAddress
	t.Cleanup(func() { dialPublicExternalInputImageAddress = originalDial })
	dialPublicExternalInputImageAddress = func(ctx context.Context, network, address string) (net.Conn, error) {
		if address != "pictures.invalid:80" {
			return nil, fmt.Errorf("unexpected address %s", address)
		}
		return (&net.Dialer{}).DialContext(ctx, network, strings.TrimPrefix(linkedImage.URL, "http://"))
	}
	encoded := base64.StdEncoding.EncodeToString(rawPNG.Bytes())
	jsonBody := fmt.Sprintf(`{"input":[{"role":"user","content":[{"type":"input_text","text":"Complete request text"},{"type":"input_image","image_url":"data:image/png;base64,%s"},{"type":"input_image","file_id":"image-only-reference"},{"type":"input_image","image_url":"http://pictures.invalid/input.png"}]}]}`, encoded)
	var form bytes.Buffer
	multipartBody := multipart.NewWriter(&form)
	if err := multipartBody.WriteField("prompt", "Complete request text"); err != nil {
		t.Fatal(err)
	}
	part, err := multipartBody.CreateFormFile("image[]", "fixture.png")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := part.Write(rawPNG.Bytes()); err != nil {
		t.Fatal(err)
	}
	if err := multipartBody.Close(); err != nil {
		t.Fatal(err)
	}

	const recordRoute = "/api/admin/prompt-filter/requests"
	request := func(path string, authorized bool) *httptest.ResponseRecorder {
		req := httptest.NewRequest(http.MethodGet, path, nil)
		if authorized {
			req.Header.Set("X-Admin-Key", "admin-secret")
		}
		rec := httptest.NewRecorder()
		router.ServeHTTP(rec, req)
		return rec
	}

	for _, tc := range []struct {
		media  string
		body   []byte
		images int
	}{
		{"application/json", []byte(jsonBody), 3},
		{multipartBody.FormDataContentType(), form.Bytes(), 1},
	} {
		id, err := db.InsertPromptRequest(ctx, database.PromptRequestBody{
			PromptRequestRecord: database.PromptRequestRecord{
				CreatedAt: time.Now().UTC(), Endpoint: "/v1/images/edits", Transport: "http",
			},
			ContentType: tc.media,
			Body:        tc.body,
		})
		if err != nil {
			t.Fatal(err)
		}
		path := fmt.Sprintf("%s/%d", recordRoute, id)
		for _, privatePath := range []string{recordRoute, path, path + "/images/0"} {
			if rec := request(privatePath, false); rec.Code != http.StatusUnauthorized {
				t.Fatalf("unauthenticated response %s: %d", privatePath, rec.Code)
			}
		}
		detail := request(path, true)
		var parsed promptRequestPresentation
		if err := json.Unmarshal(detail.Body.Bytes(), &parsed); err != nil || detail.Code != http.StatusOK {
			t.Fatalf("detail status=%d body=%s err=%v", detail.Code, detail.Body, err)
		}
		if len(parsed.Images) != tc.images || !strings.Contains(parsed.Text, "Complete request text") || strings.Contains(parsed.Text, encoded) {
			t.Fatalf("detail images=%+v text=%s", parsed.Images, parsed.Text)
		}
		if tc.images > 1 {
			if parsed.Images[1].Source != "reference" || parsed.Images[2].Source != "url" {
				t.Fatalf("unexpected image references: %+v", parsed.Images)
			}
			if rec := request(path+"/images/1", true); rec.Code != http.StatusNotFound {
				t.Fatalf("file reference status=%d", rec.Code)
			}
			if rec := request(path+"/images/2", true); rec.Code != http.StatusOK || !bytes.Equal(rec.Body.Bytes(), rawPNG.Bytes()) {
				t.Fatalf("linked image status=%d", rec.Code)
			}
		}
		preview := request(path+"/images/0", true)
		if preview.Code != http.StatusOK || preview.Header().Get("Content-Type") != "image/png" ||
			preview.Header().Get("Cache-Control") != "private, no-store" || !bytes.Equal(preview.Body.Bytes(), rawPNG.Bytes()) {
			t.Fatalf("preview status=%d headers=%v", preview.Code, preview.Header())
		}
	}
	page := request(recordRoute, true)
	if page.Code != http.StatusOK || strings.Contains(page.Body.String(), "Complete request text") {
		t.Fatalf("list exposes details or failed: %d %s", page.Code, page.Body)
	}
	if err := db.PurgeExpiredPromptRequests(ctx, time.Now().AddDate(0, 0, 31)); err != nil {
		t.Fatal(err)
	}
	if page := request(recordRoute, true); !strings.Contains(page.Body.String(), `"records":[]`) {
		t.Fatalf("records not removed by retention: %s", page.Body)
	}
}

func TestPromptRequestRetentionOptionsAndCleanup(t *testing.T) {
	handler, db := newTestAdminHandlerFixture(t)
	router := gin.New()
	handler.RegisterRoutes(router)
	ctx := context.Background()
	get := func() int {
		t.Helper()
		rec := httptest.NewRecorder()
		req := httptest.NewRequest(http.MethodGet, "/api/admin/prompt-filter/requests", nil)
		req.Header.Set("X-Admin-Key", "admin-secret")
		router.ServeHTTP(rec, req)
		var out struct {
			Days int `json:"retention_days"`
		}
		if err := json.Unmarshal(rec.Body.Bytes(), &out); err != nil || rec.Code != http.StatusOK {
			t.Fatalf("read retention status=%d body=%s err=%v", rec.Code, rec.Body, err)
		}
		return out.Days
	}
	put := func(days, expected int) {
		t.Helper()
		rec := httptest.NewRecorder()
		req := httptest.NewRequest(http.MethodPut, "/api/admin/prompt-filter/requests/retention",
			strings.NewReader(fmt.Sprintf(`{"retention_days":%d}`, days)))
		req.Header.Set("Content-Type", "application/json")
		req.Header.Set("X-Admin-Key", "admin-secret")
		router.ServeHTTP(rec, req)
		if rec.Code != expected {
			t.Fatalf("retention %d: status=%d body=%s, want %d", days, rec.Code, rec.Body, expected)
		}
	}
	if got := get(); got != 30 {
		t.Fatalf("default retention=%d, want 30", got)
	}
	for _, days := range []int{90, 7, 1, 30} {
		put(days, http.StatusOK)
		if got := get(); got != days {
			t.Fatalf("saved retention=%d, want %d", got, days)
		}
	}
	put(14, http.StatusBadRequest)
	if got := get(); got != 30 {
		t.Fatalf("invalid update changed retention to %d", got)
	}
	now := time.Now().UTC()
	for _, days := range []int{2, 0} {
		_, err := db.InsertPromptRequest(ctx, database.PromptRequestBody{
			PromptRequestRecord: database.PromptRequestRecord{CreatedAt: now.AddDate(0, 0, -days), Transport: "http"},
			Body:                []byte(`{"input":"test"}`),
		})
		if err != nil {
			t.Fatal(err)
		}
	}
	for _, check := range []struct{ days, count int }{{7, 2}, {1, 1}} {
		put(check.days, http.StatusOK)
		if err := db.PurgeExpiredPromptRequests(ctx, now); err != nil {
			t.Fatal(err)
		}
		rows, err := db.ListPromptRequests(ctx, 0, 10)
		if err != nil || len(rows) != check.count {
			t.Fatalf("retention %d left %d records with error %v, want %d", check.days, len(rows), err, check.count)
		}
	}
}
