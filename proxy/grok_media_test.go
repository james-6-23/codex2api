package proxy

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"net/http/httptrace"
	"net/textproto"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/codex2api/auth"
	"github.com/codex2api/cache"
	"github.com/codex2api/database"
	"github.com/gin-gonic/gin"
	"github.com/tidwall/gjson"
)

func TestGrokMediaModelClassification(t *testing.T) {
	for model, wantImage := range map[string]bool{
		"grok-imagine":               true,
		"grok-imagine-image":         true,
		"grok-imagine-image-quality": true,
		"grok-imagine-video":         false,
		"grok-4.6":                   false,
		"gpt-image-2":                false,
	} {
		if got := isGrokImageModel(model); got != wantImage {
			t.Errorf("isGrokImageModel(%q) = %v, want %v", model, got, wantImage)
		}
	}
	if !isGrokVideoModel("grok-imagine-video-1.5-preview") || isGrokVideoModel("grok-imagine-image") {
		t.Error("isGrokVideoModel misclassified")
	}
	if !isMediaOnlyModel("gpt-image-2") || !isMediaOnlyModel("grok-imagine-video") || isMediaOnlyModel("grok-4.6") {
		t.Error("isMediaOnlyModel misclassified")
	}
	if got := normalizeGrokMediaModel("grok-imagine"); got != grokImagineImageQualityModel {
		t.Errorf("normalizeGrokMediaModel(grok-imagine) = %q", got)
	}
}

func TestMapGrokMediaModelForProfile(t *testing.T) {
	if got := mapGrokMediaModelForProfile(grokImagineVideo15Model, grokMediaProfileXAI); got != grokImagineVideo15Preview {
		t.Errorf("xai profile model = %q, want -preview", got)
	}
	if got := mapGrokMediaModelForProfile(grokImagineVideo15Preview, grokMediaProfileCLI); got != grokImagineVideo15Model {
		t.Errorf("cli profile model = %q, want without -preview", got)
	}
	if got := mapGrokMediaModelForProfile("grok-imagine-image", grokMediaProfileXAI); got != "grok-imagine-image" {
		t.Errorf("image model must be unchanged, got %q", got)
	}
}

// 媒体模型集是独立能力轴:未声明白名单或只声明文本模型的账号得到默认媒体集
// (账号导入会把文本目录写进白名单,不能因此关闭媒体);白名单里显式出现
// grok-imagine 条目时才对媒体收窄。
func TestGrokMediaModelsForAccount(t *testing.T) {
	open := &auth.Account{DBID: 1, UpstreamType: auth.UpstreamGrok, APIKey: "xai"}
	models := grokMediaModelsForAccount(open)
	if !modelIDInList("grok-imagine-image-quality", models) || !modelIDInList(grokImagineVideo15Model, models) {
		t.Fatalf("default media set missing entries: %v", models)
	}

	narrowed := &auth.Account{DBID: 2, UpstreamType: auth.UpstreamGrok, APIKey: "xai", Models: []string{"grok-4.6", "grok-imagine-video-1.5"}}
	models = grokMediaModelsForAccount(narrowed)
	if len(models) != 1 || !modelIDInList(grokImagineVideo15Model, models) {
		t.Fatalf("declared media models must narrow, got %v", models)
	}

	textOnly := &auth.Account{DBID: 3, UpstreamType: auth.UpstreamGrok, APIKey: "xai", Models: []string{"grok-4.6"}}
	models = grokMediaModelsForAccount(textOnly)
	if !modelIDInList("grok-imagine-image", models) {
		t.Fatalf("text-only whitelist must not close media capability, got %v", models)
	}

	if !grokMediaAccountSupportsModel(open, "grok-imagine-image") || !grokMediaAccountSupportsModel(textOnly, "grok-imagine-image") {
		t.Error("grokMediaAccountSupportsModel admission mismatch")
	}
	if grokMediaAccountSupportsModel(narrowed, "grok-imagine-image") {
		t.Error("declared media whitelist must narrow admission")
	}
}

func TestGrokMediaProfilesForAccount(t *testing.T) {
	apiKey := &auth.Account{DBID: 1, UpstreamType: auth.UpstreamGrok, APIKey: "xai-key"}
	profiles := grokMediaProfilesForAccount(apiKey)
	if len(profiles) != 1 || profiles[0].Kind != grokMediaProfileXAI {
		t.Fatalf("api-key profiles = %+v, want single xai", profiles)
	}

	oauth := &auth.Account{DBID: 2, UpstreamType: auth.UpstreamGrok, AccessToken: "at"}
	profiles = grokMediaProfilesForAccount(oauth)
	if len(profiles) != 2 || profiles[0].Kind != grokMediaProfileCLI || profiles[1].Kind != grokMediaProfileXAI {
		t.Fatalf("oauth profiles = %+v, want cli then xai fallback", profiles)
	}
	if profiles[1].BaseURL != auth.GrokDefaultAPIBaseURL {
		t.Errorf("oauth fallback base = %q", profiles[1].BaseURL)
	}
}

func TestBuildGrokImagesBody(t *testing.T) {
	params := grokImagesParamsFromJSON([]byte(`{"n":2,"size":"2048x2048","quality":"low"}`))
	if params.Resolution != "2k" {
		t.Fatalf("size 2048 must map to resolution 2k, got %q", params.Resolution)
	}
	body := buildGrokImagesBody("grok-imagine-image-quality", "a cat", "url", params, []string{"data:image/png;base64,AA=="})
	if gjson.GetBytes(body, "size").Exists() || gjson.GetBytes(body, "stream").Exists() {
		t.Error("upstream body must not contain size/stream")
	}
	if gjson.GetBytes(body, "resolution").String() != "2k" || gjson.GetBytes(body, "n").Int() != 2 {
		t.Errorf("body = %s", body)
	}
	if gjson.GetBytes(body, "images.0.url").String() == "" || gjson.GetBytes(body, "images.0.type").String() != "image_url" {
		t.Errorf("images entry malformed: %s", body)
	}
}

func TestBuildGrokVideoBodyProfileFieldNaming(t *testing.T) {
	raw := []byte(`{"model":"grok-imagine-video-1.5","prompt":"waves","duration":"8","resolution":"720P","image":{"url":"https://example.com/a.png"},"reference_images":["https://example.com/b.png"],"output":{"upload_url":"https://evil.example"},"storage_options":{"x":1}}`)

	cli := buildGrokVideoBody(raw, grokImagineVideo15Model, grokMediaProfileCLI)
	if gjson.GetBytes(cli, "image.image_url").String() == "" || gjson.GetBytes(cli, "image.url").Exists() {
		t.Errorf("cli profile image field = %s", cli)
	}
	if gjson.GetBytes(cli, "reference_images.0.image_url").String() == "" {
		t.Errorf("cli profile reference field = %s", cli)
	}

	xai := buildGrokVideoBody(raw, grokImagineVideo15Preview, grokMediaProfileXAI)
	if gjson.GetBytes(xai, "image.url").String() == "" || gjson.GetBytes(xai, "image.image_url").Exists() {
		t.Errorf("xai profile image field = %s", xai)
	}
	if gjson.GetBytes(xai, "duration").Int() != 8 {
		t.Errorf("duration string must parse to int, body = %s", xai)
	}
	if gjson.GetBytes(xai, "resolution").String() != "720p" {
		t.Errorf("resolution must lowercase, body = %s", xai)
	}
	for _, field := range []string{"output", "storage_options"} {
		if gjson.GetBytes(xai, field).Exists() {
			t.Errorf("unsupported field %s must be dropped", field)
		}
	}
}

func TestIsTrustedGrokVideoAssetURL(t *testing.T) {
	for raw, want := range map[string]bool{
		"https://vidgen.x.ai/video/abc.mp4":     true,
		"https://cdn.vidgen.x.ai/video/abc.mp4": true,
		"https://assets.grok.com/video.mp4":     true,
		"http://vidgen.x.ai/video.mp4":          false,
		"https://vidgen.x.ai:8443/video.mp4":    false,
		"https://user@vidgen.x.ai/video.mp4":    false,
		"https://evilvidgen.x.ai.example.com/a": false,
		"https://example.com/vidgen.x.ai/a.mp4": false,
		"":                                      false,
		"https://notvidgen.x.ai.evil.com/a.mp4": false,
		"https://vidgen.x.ai.evil.com/a.mp4":    false,
		"https://sub.assets.grok.com/video.mp4": true,
	} {
		if got := isTrustedGrokVideoAssetURL(raw); got != want {
			t.Errorf("isTrustedGrokVideoAssetURL(%q) = %v, want %v", raw, got, want)
		}
	}
}

func TestValidGrokVideoRequestID(t *testing.T) {
	for id, want := range map[string]bool{
		"video_abc-123":          true,
		"":                       false,
		".":                      false,
		"..":                     false,
		"a/b":                    false,
		"a\\b":                   false,
		"a b":                    false,
		strings.Repeat("x", 201): false,
	} {
		if got := validGrokVideoRequestID(id); got != want {
			t.Errorf("validGrokVideoRequestID(%q) = %v, want %v", id, got, want)
		}
	}
}

func TestValidateImagesModelAcceptsGrokImagine(t *testing.T) {
	if err := validateImagesModel("grok-imagine-image"); err != nil {
		t.Errorf("grok image model rejected: %v", err)
	}
	if err := validateImagesModel("grok-imagine"); err != nil {
		t.Errorf("grok-imagine alias rejected: %v", err)
	}
	if err := validateImagesModel("grok-imagine-video"); err == nil || !strings.Contains(err.Error(), "/v1/videos/generations") {
		t.Errorf("video model must be redirected to videos endpoint, err = %v", err)
	}
	if err := validateImagesModel("grok-4.6"); err == nil {
		t.Error("text model must be rejected")
	}
}

func newGrokMediaTestHandler(t *testing.T, account *auth.Account) *Handler {
	t.Helper()
	store := auth.NewStore(nil, nil, &database.SystemSettings{MaxConcurrency: 2})
	store.AddAccount(account)
	handler := NewHandler(store, nil, nil, nil)
	handler.SetRuntimeCache(cache.NewMemory(1))
	return handler
}

func newGrokMediaTestContext(t *testing.T, method, target string, body []byte) (*gin.Context, *httptest.ResponseRecorder) {
	t.Helper()
	gin.SetMode(gin.TestMode)
	rec := httptest.NewRecorder()
	ctx, _ := gin.CreateTestContext(rec)
	var reader io.Reader
	if body != nil {
		reader = bytes.NewReader(body)
	}
	req := httptest.NewRequest(method, target, reader)
	req.Header.Set("Content-Type", "application/json")
	ctx.Request = req
	ctx.Set(contextAPIKeyID, int64(11))
	return ctx, rec
}

// 生图:请求直投上游 images 端点,响应原样透传;上游收到的 body 不含 size。
func TestGrokImagesGenerationsPassthrough(t *testing.T) {
	var seenPath string
	var seenBody []byte
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		seenPath = r.URL.Path
		seenBody = readUpstreamRequestBody(r)
		if got := r.Header.Get("Authorization"); got != "Bearer xai-test" {
			t.Errorf("Authorization = %q", got)
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"data":[{"url":"https://imgen.x.ai/a.png"}]}`))
	}))
	defer upstream.Close()

	account := &auth.Account{DBID: 1, UpstreamType: auth.UpstreamGrok, APIKey: "xai-test", BaseURL: upstream.URL}
	handler := newGrokMediaTestHandler(t, account)

	ctx, rec := newGrokMediaTestContext(t, http.MethodPost, "/v1/images/generations", []byte(`{"model":"grok-imagine","prompt":"a cat","size":"1024x1024","response_format":"url"}`))
	handler.ImagesGenerations(ctx)

	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, body = %s", rec.Code, rec.Body.String())
	}
	if seenPath != "/v1/images/generations" {
		t.Errorf("upstream path = %q", seenPath)
	}
	if gjson.GetBytes(seenBody, "model").String() != grokImagineImageQualityModel {
		t.Errorf("upstream model = %s", gjson.GetBytes(seenBody, "model").String())
	}
	if gjson.GetBytes(seenBody, "size").Exists() {
		t.Errorf("size must not reach upstream: %s", seenBody)
	}
	if got := gjson.Get(rec.Body.String(), "data.0.url").String(); got != "https://imgen.x.ai/a.png" {
		t.Errorf("response not passthrough: %s", rec.Body.String())
	}
}

func TestGrokMediaCatchAllRetriesBeyondOrdinaryAttemptCap(t *testing.T) {
	previousRuntime := CurrentRuntimeSettings()
	t.Cleanup(func() { ApplyRuntimeSettings(previousRuntime) })
	nextRuntime := previousRuntime
	nextRuntime.ContinuousRetryPolicy = database.ContinuousRetryPolicy{Enabled: true, CatchAll: true}
	ApplyRuntimeSettings(nextRuntime)

	tests := []struct {
		name        string
		path        string
		requestBody string
		successBody string
		invoke      func(*Handler, *gin.Context)
	}{
		{
			name:        "image creation",
			path:        "/v1/images/generations",
			requestBody: `{"model":"grok-imagine-image","prompt":"a test image"}`,
			successBody: `{"data":[{"url":"https://imgen.x.ai/test.png"}]}`,
			invoke:      (*Handler).ImagesGenerations,
		},
		{
			name:        "video creation",
			path:        "/v1/videos/generations",
			requestBody: `{"model":"grok-imagine-video-1.5","prompt":"a test video"}`,
			successBody: `{"request_id":"video_retry_success"}`,
			invoke:      (*Handler).VideosGenerations,
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			var calls atomic.Int32
			upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
				if calls.Add(1) <= maxGrokMediaAttempts {
					w.Header().Set("Content-Type", "application/json")
					w.WriteHeader(http.StatusTeapot)
					_, _ = io.WriteString(w, `{"error":{"code":"future_media_failure","message":"must stay upstream"}}`)
					return
				}
				w.Header().Set("Content-Type", "application/json")
				_, _ = io.WriteString(w, tc.successBody)
			}))
			t.Cleanup(upstream.Close)

			account := &auth.Account{DBID: 1, UpstreamType: auth.UpstreamGrok, APIKey: "xai-test", BaseURL: upstream.URL}
			handler := newGrokMediaTestHandler(t, account)
			t.Cleanup(handler.store.Stop)
			ctx, rec := newGrokMediaTestContext(t, http.MethodPost, tc.path, []byte(tc.requestBody))
			requestCtx, cancel := context.WithTimeout(ctx.Request.Context(), 8*time.Second)
			defer cancel()
			ctx.Request = ctx.Request.WithContext(requestCtx)

			tc.invoke(handler, ctx)

			if got := calls.Load(); got != maxGrokMediaAttempts+1 {
				t.Fatalf("upstream calls = %d, want %d failures followed by success", got, maxGrokMediaAttempts+1)
			}
			if rec.Code != http.StatusOK || strings.Contains(rec.Body.String(), "must stay upstream") {
				t.Fatalf("retry was not transparent: status=%d body=%s", rec.Code, rec.Body.String())
			}
		})
	}
}

func TestGrokMediaContinuousRetrySendsHTTP102DuringUpstreamWait(t *testing.T) {
	previousRuntime := CurrentRuntimeSettings()
	previousKeepaliveInterval := continuousRetryKeepaliveInterval
	t.Cleanup(func() {
		ApplyRuntimeSettings(previousRuntime)
		continuousRetryKeepaliveInterval = previousKeepaliveInterval
	})
	nextRuntime := previousRuntime
	nextRuntime.ContinuousRetryPolicy = database.ContinuousRetryPolicy{
		Enabled:            true,
		CatchAll:           true,
		MaxDurationSeconds: 2,
	}
	ApplyRuntimeSettings(nextRuntime)
	continuousRetryKeepaliveInterval = 5 * time.Millisecond

	var calls atomic.Int32
	secondStarted := make(chan struct{})
	var secondOnce sync.Once
	releaseSecond := make(chan struct{})
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		switch calls.Add(1) {
		case 1:
			w.Header().Set("Content-Type", "application/json")
			w.WriteHeader(http.StatusInternalServerError)
			_, _ = io.WriteString(w, `{"error":{"code":"retry_me"}}`)
		case 2:
			secondOnce.Do(func() { close(secondStarted) })
			<-releaseSecond
			w.Header().Set("Content-Type", "application/json")
			_, _ = io.WriteString(w, `{"data":[{"url":"https://imgen.x.ai/recovered.png"}]}`)
		default:
			w.WriteHeader(http.StatusInternalServerError)
		}
	}))
	t.Cleanup(upstream.Close)

	account := &auth.Account{DBID: 1, UpstreamType: auth.UpstreamGrok, APIKey: "xai-test", BaseURL: upstream.URL}
	handler := newGrokMediaTestHandler(t, account)
	t.Cleanup(handler.store.Stop)
	gateway := gin.New()
	gateway.Use(func(c *gin.Context) {
		c.Set(contextAPIKeyID, int64(11))
		c.Next()
	})
	gateway.POST("/v1/images/generations", handler.ImagesGenerations)
	server := httptest.NewServer(gateway)
	t.Cleanup(server.Close)

	informational := make(chan struct{}, 128)
	trace := &httptrace.ClientTrace{Got1xxResponse: func(code int, _ textproto.MIMEHeader) error {
		if code == http.StatusProcessing {
			select {
			case informational <- struct{}{}:
			default:
			}
		}
		return nil
	}}
	requestContext, cancel := context.WithTimeout(httptrace.WithClientTrace(context.Background(), trace), 2*time.Second)
	defer cancel()
	request, err := http.NewRequestWithContext(requestContext, http.MethodPost, server.URL+"/v1/images/generations", strings.NewReader(`{"model":"grok-imagine-image","prompt":"keep this request alive"}`))
	if err != nil {
		t.Fatalf("build request: %v", err)
	}
	request.Header.Set("Content-Type", "application/json")
	result := make(chan struct {
		response *http.Response
		err      error
	}, 1)
	go func() {
		response, requestErr := server.Client().Do(request)
		result <- struct {
			response *http.Response
			err      error
		}{response: response, err: requestErr}
	}()

	select {
	case <-secondStarted:
	case <-time.After(time.Second):
		close(releaseSecond)
		t.Fatal("second Grok media attempt did not start")
	}
	for {
		select {
		case <-informational:
			continue
		default:
			break
		}
		break
	}
	select {
	case <-informational:
		close(releaseSecond)
	case <-time.After(time.Second):
		close(releaseSecond)
		t.Fatal("Grok media upstream wait emitted no HTTP 102 heartbeat")
	}
	callResult := <-result
	if callResult.err != nil {
		t.Fatalf("gateway request: %v", callResult.err)
	}
	defer callResult.response.Body.Close()
	if callResult.response.StatusCode != http.StatusOK {
		body, _ := io.ReadAll(callResult.response.Body)
		t.Fatalf("gateway status = %d, body = %s", callResult.response.StatusCode, body)
	}
	body, err := io.ReadAll(callResult.response.Body)
	if err != nil {
		t.Fatalf("read gateway response: %v", err)
	}
	if !strings.Contains(string(body), "recovered.png") {
		t.Fatalf("gateway response = %s", body)
	}
	if got := calls.Load(); got != 2 {
		t.Fatalf("upstream calls = %d, want 2", got)
	}
}

func TestGrokMediaRetriesFailuresHiddenInsideHTTP200(t *testing.T) {
	tests := []struct {
		name        string
		policy      database.ContinuousRetryPolicy
		path        string
		requestBody string
		failureBody string
		successBody string
		invoke      func(*Handler, *gin.Context)
	}{
		{
			name:        "catch-all image malformed success",
			policy:      database.ContinuousRetryPolicy{Enabled: true, CatchAll: true},
			path:        "/v1/images/generations",
			requestBody: `{"model":"grok-imagine-image","prompt":"a test image"}`,
			failureBody: `{"unexpected":"shape"}`,
			successBody: `{"data":[{"url":"https://imgen.x.ai/test.png"}]}`,
			invoke:      (*Handler).ImagesGenerations,
		},
		{
			name:        "selective video error code",
			policy:      database.ContinuousRetryPolicy{Enabled: true, ErrorCodes: []string{"future_media_failure"}},
			path:        "/v1/videos/generations",
			requestBody: `{"model":"grok-imagine-video-1.5","prompt":"a test video"}`,
			failureBody: `{"error":{"code":"future_media_failure","message":"must stay upstream"}}`,
			successBody: `{"request_id":"video_retry_success"}`,
			invoke:      (*Handler).VideosGenerations,
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			previousRuntime := CurrentRuntimeSettings()
			t.Cleanup(func() { ApplyRuntimeSettings(previousRuntime) })
			nextRuntime := previousRuntime
			nextRuntime.ContinuousRetryPolicy = tc.policy
			ApplyRuntimeSettings(nextRuntime)

			var calls atomic.Int32
			upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
				w.Header().Set("Content-Type", "application/json")
				if calls.Add(1) == 1 {
					_, _ = io.WriteString(w, tc.failureBody)
					return
				}
				_, _ = io.WriteString(w, tc.successBody)
			}))
			t.Cleanup(upstream.Close)

			account := &auth.Account{DBID: 1, UpstreamType: auth.UpstreamGrok, APIKey: "xai-test", BaseURL: upstream.URL}
			handler := newGrokMediaTestHandler(t, account)
			t.Cleanup(handler.store.Stop)
			ctx, rec := newGrokMediaTestContext(t, http.MethodPost, tc.path, []byte(tc.requestBody))
			requestCtx, cancel := context.WithTimeout(ctx.Request.Context(), 5*time.Second)
			defer cancel()
			ctx.Request = ctx.Request.WithContext(requestCtx)

			tc.invoke(handler, ctx)

			if got := calls.Load(); got != 2 {
				t.Fatalf("upstream calls = %d, want 2", got)
			}
			if rec.Code != http.StatusOK || strings.Contains(rec.Body.String(), "must stay upstream") {
				t.Fatalf("retry was not transparent: status=%d body=%s", rec.Code, rec.Body.String())
			}
		})
	}
}

// 生视频端到端:创建返回 request_id 并绑定账号;状态查询把 video.url 重写为网关
// content 代理地址;content 下载经网关流式转发。
func TestGrokVideoCreateStatusContentFlow(t *testing.T) {
	const videoPayload = "fake-mp4-bytes"
	var assetURL string
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case r.Method == http.MethodPost && r.URL.Path == "/v1/videos/generations":
			body := readUpstreamRequestBody(r)
			// API Key 账号走 xai profile:1.5 模型名须归一成 -preview。
			if got := gjson.GetBytes(body, "model").String(); got != grokImagineVideo15Preview {
				t.Errorf("upstream model = %q", got)
			}
			w.Header().Set("Content-Type", "application/json")
			_, _ = w.Write([]byte(`{"request_id":"video_test123"}`))
		case r.Method == http.MethodGet && r.URL.Path == "/v1/videos/video_test123":
			w.Header().Set("Content-Type", "application/json")
			_, _ = w.Write([]byte(`{"status":"done","progress":100,"video":{"url":"` + assetURL + `","duration":8}}`))
		case r.Method == http.MethodGet && r.URL.Path == "/v1/videos/video_test123/content":
			w.Header().Set("Content-Type", "video/mp4")
			_, _ = w.Write([]byte(videoPayload))
		default:
			t.Errorf("unexpected upstream call %s %s", r.Method, r.URL.Path)
			w.WriteHeader(http.StatusNotFound)
		}
	}))
	defer upstream.Close()
	// 测试上游是 http 且非官方资产域,签名 URL 不可信,content 会走带凭据的回退路径。
	assetURL = upstream.URL + "/signed/video.mp4"

	account := &auth.Account{DBID: 1, UpstreamType: auth.UpstreamGrok, APIKey: "xai-test", BaseURL: upstream.URL}
	handler := newGrokMediaTestHandler(t, account)

	// 1. 创建
	ctx, rec := newGrokMediaTestContext(t, http.MethodPost, "/v1/videos/generations", []byte(`{"model":"grok-imagine-video-1.5","prompt":"waves","duration":8,"resolution":"720p"}`))
	handler.VideosGenerations(ctx)
	if rec.Code != http.StatusOK {
		t.Fatalf("create status = %d, body = %s", rec.Code, rec.Body.String())
	}
	if got := gjson.Get(rec.Body.String(), "request_id").String(); got != "video_test123" {
		t.Fatalf("request_id = %q", got)
	}

	// 2. 状态查询:video.url 必须被重写为网关 content 代理地址
	ctx, rec = newGrokMediaTestContext(t, http.MethodGet, "/v1/videos/video_test123", nil)
	ctx.Params = gin.Params{{Key: "request_id", Value: "video_test123"}}
	handler.VideosStatus(ctx)
	if rec.Code != http.StatusOK {
		t.Fatalf("status status = %d, body = %s", rec.Code, rec.Body.String())
	}
	rewritten := gjson.Get(rec.Body.String(), "video.url").String()
	if !strings.HasSuffix(rewritten, "/v1/videos/video_test123/content") {
		t.Fatalf("video.url = %q, want gateway content proxy", rewritten)
	}
	if strings.Contains(rewritten, upstream.URL) {
		t.Fatalf("video.url must not leak upstream address: %q", rewritten)
	}
	if got := gjson.Get(rec.Body.String(), "status").String(); got != "done" {
		t.Errorf("status field = %q", got)
	}

	// 3. content 下载:经网关代理转发
	ctx, rec = newGrokMediaTestContext(t, http.MethodGet, "/v1/videos/video_test123/content", nil)
	ctx.Params = gin.Params{{Key: "request_id", Value: "video_test123"}}
	handler.VideosContent(ctx)
	if rec.Code != http.StatusOK {
		t.Fatalf("content status = %d", rec.Code)
	}
	if rec.Body.String() != videoPayload {
		t.Errorf("content body = %q", rec.Body.String())
	}
	if got := rec.Header().Get("Content-Type"); got != "video/mp4" {
		t.Errorf("content type = %q", got)
	}
}

// 状态/下载必须命中创建任务的 API Key;其他 Key 一律 404,不泄露任务存在性。
func TestGrokVideoBindingOwnership(t *testing.T) {
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"request_id":"video_owned"}`))
	}))
	defer upstream.Close()

	account := &auth.Account{DBID: 1, UpstreamType: auth.UpstreamGrok, APIKey: "xai-test", BaseURL: upstream.URL}
	handler := newGrokMediaTestHandler(t, account)

	ctx, rec := newGrokMediaTestContext(t, http.MethodPost, "/v1/videos/generations", []byte(`{"prompt":"waves"}`))
	handler.VideosGenerations(ctx)
	if rec.Code != http.StatusOK {
		t.Fatalf("create status = %d, body = %s", rec.Code, rec.Body.String())
	}

	ctx, rec = newGrokMediaTestContext(t, http.MethodGet, "/v1/videos/video_owned", nil)
	ctx.Set(contextAPIKeyID, int64(99)) // 另一个 Key
	ctx.Params = gin.Params{{Key: "request_id", Value: "video_owned"}}
	handler.VideosStatus(ctx)
	if rec.Code != http.StatusNotFound {
		t.Fatalf("cross-key status = %d, want 404", rec.Code)
	}
}

// 上游报错时透传错误且只做模型级冷却,不牵连账号的文本调度。
func TestGrokMediaUpstreamErrorModelLevelCooldown(t *testing.T) {
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusForbidden)
		_, _ = w.Write([]byte(`{"error":"media not allowed for this plan"}`))
	}))
	defer upstream.Close()

	account := &auth.Account{DBID: 1, UpstreamType: auth.UpstreamGrok, APIKey: "xai-test", BaseURL: upstream.URL}
	handler := newGrokMediaTestHandler(t, account)

	ctx, rec := newGrokMediaTestContext(t, http.MethodPost, "/v1/images/generations", []byte(`{"model":"grok-imagine-image","prompt":"a cat"}`))
	handler.ImagesGenerations(ctx)

	// 上游账号级 403 按网关既有语义改写为池级 503(sendFinalUpstreamError),
	// 不把账号侧的套餐/权限问题当成下游客户端错误。
	if rec.Code != http.StatusServiceUnavailable || !strings.Contains(rec.Body.String(), "account_pool_forbidden") {
		t.Fatalf("status = %d, body = %s, want 503 account_pool_forbidden", rec.Code, rec.Body.String())
	}
	if !account.IsModelRateLimited("grok-imagine-image") {
		t.Error("403 must trigger model-level cooldown")
	}
	if account.IsModelRateLimited("grok-4.6") {
		t.Error("text models must not be affected")
	}
	if !account.IsAvailable() {
		t.Error("account must stay available for text traffic")
	}
}

// 无前缀路由下状态响应的代理地址不带 /v1 前缀,与请求形态一致。
func TestGrokVideoContentProxyURLPrefix(t *testing.T) {
	gin.SetMode(gin.TestMode)
	rec := httptest.NewRecorder()
	ctx, _ := gin.CreateTestContext(rec)
	ctx.Request = httptest.NewRequest(http.MethodGet, "/videos/abc", nil)
	ctx.Request.Host = "gw.example.com"
	ctx.Request.Header.Set("X-Forwarded-Proto", "https")
	if got := grokVideoContentProxyURL(ctx, "abc"); got != "https://gw.example.com/videos/abc/content" {
		t.Errorf("proxy url = %q", got)
	}

	ctx.Request = httptest.NewRequest(http.MethodGet, "/v1/videos/abc", nil)
	ctx.Request.Host = "gw.example.com"
	if got := grokVideoContentProxyURL(ctx, "abc"); got != "http://gw.example.com/v1/videos/abc/content" {
		t.Errorf("proxy url = %q", got)
	}
}

// 生视频请求体缺 prompt 且无图片输入时 400,不消耗账号额度。
func TestGrokVideoCreateValidation(t *testing.T) {
	account := &auth.Account{DBID: 1, UpstreamType: auth.UpstreamGrok, APIKey: "xai-test"}
	handler := newGrokMediaTestHandler(t, account)

	ctx, rec := newGrokMediaTestContext(t, http.MethodPost, "/v1/videos/generations", []byte(`{}`))
	handler.VideosGenerations(ctx)
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("empty create status = %d, want 400", rec.Code)
	}

	ctx, rec = newGrokMediaTestContext(t, http.MethodPost, "/v1/videos/edits", []byte(`{"prompt":"x"}`))
	handler.VideosEdits(ctx)
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("edits without video status = %d, want 400", rec.Code)
	}

	ctx, rec = newGrokMediaTestContext(t, http.MethodPost, "/v1/videos/generations", []byte(`{"model":"grok-4.6","prompt":"x"}`))
	handler.VideosGenerations(ctx)
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("text model on videos endpoint status = %d, want 400", rec.Code)
	}
}

func TestGrokVideoCreateRejectsStream(t *testing.T) {
	account := &auth.Account{DBID: 1, UpstreamType: auth.UpstreamGrok, APIKey: "xai-test"}
	handler := newGrokMediaTestHandler(t, account)
	t.Cleanup(handler.store.Stop)

	ctx, rec := newGrokMediaTestContext(t, http.MethodPost, "/v1/videos/generations", []byte(`{"model":"grok-imagine-video-1.5","prompt":"x","stream":true}`))
	handler.VideosGenerations(ctx)
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("stream video status = %d, want 400", rec.Code)
	}
	if !strings.Contains(rec.Body.String(), "stream is not supported") {
		t.Fatalf("stream video error = %s", rec.Body.String())
	}
}

func TestGrokVideoBindingRoundTrip(t *testing.T) {
	handler := newGrokMediaTestHandler(t, &auth.Account{DBID: 5, UpstreamType: auth.UpstreamGrok, APIKey: "xai"})
	binding := grokVideoBinding{AccountID: 5, APIKeyID: 11, Profile: grokMediaProfileXAI, Model: grokImagineVideo15Preview, CreatedAt: 1700000000}
	handler.storeGrokVideoBinding(t.Context(), "video_rt", binding)
	loaded, ok := handler.loadGrokVideoBinding(t.Context(), "video_rt")
	if !ok {
		t.Fatal("binding not found after store")
	}
	if loaded != binding {
		t.Errorf("binding = %+v, want %+v", loaded, binding)
	}
	raw, _ := json.Marshal(binding)
	if !bytes.Contains(raw, []byte(`"account_id":5`)) {
		t.Errorf("binding json = %s", raw)
	}
}

// 媒体调度优先付费凭据:free OAuth 号打媒体端点必 403,不该在首选层出现;
// API Key 账号按量计费视同付费。
func TestGrokMediaPreferredAccountFilter(t *testing.T) {
	filter := grokMediaPreferredAccountFilter("grok-imagine-image")
	free := &auth.Account{DBID: 1, UpstreamType: auth.UpstreamGrok, AccessToken: "at", PlanType: "free"}
	heavy := &auth.Account{DBID: 2, UpstreamType: auth.UpstreamGrok, AccessToken: "at", PlanType: "supergrok_heavy"}
	unknown := &auth.Account{DBID: 3, UpstreamType: auth.UpstreamGrok, AccessToken: "at"}
	apiKey := &auth.Account{DBID: 4, UpstreamType: auth.UpstreamGrok, APIKey: "xai-key", PlanType: "free"}
	if filter(free) {
		t.Error("free plan must not be preferred")
	}
	if !filter(heavy) {
		t.Error("paid plan must be preferred")
	}
	if filter(unknown) {
		t.Error("unknown plan oauth must fall to the fallback tier")
	}
	if !filter(apiKey) {
		t.Error("api-key credential must be preferred regardless of plan")
	}
	// 首选层是基础媒体过滤器的子集:非 Grok 账号仍被拒
	if filter(&auth.Account{DBID: 5, AccessToken: "codex", PlanType: "pro"}) {
		t.Error("non-grok account must be rejected")
	}
}

func TestGrokImagesUsageLogInfo(t *testing.T) {
	urlResp := []byte(`{"data":[{"url":"https://imgen.x.ai/a.jpeg","mime_type":"image/jpeg"},{"url":"https://imgen.x.ai/b.jpeg","mime_type":"image/jpeg"}]}`)
	info := grokImagesUsageLogInfo(urlResp)
	if info.Count != 2 || info.Format != "jpeg" {
		t.Fatalf("url form info = %+v, want count 2 format jpeg", info)
	}
	// 1x1 PNG,可解出尺寸与字节数
	const tinyPNG = "iVBORw0KGgoAAAANSUhEUgAAAAEAAAABCAYAAAAfFcSJAAAADUlEQVR42mP8z8BQDwAEhQGAhKmMIQAAAABJRU5ErkJggg=="
	b64Resp := []byte(`{"data":[{"b64_json":"` + tinyPNG + `"}]}`)
	info = grokImagesUsageLogInfo(b64Resp)
	if info.Count != 1 || info.Width != 1 || info.Height != 1 || info.Bytes <= 0 {
		t.Fatalf("b64 form info = %+v, want 1x1 with bytes", info)
	}
	if info = grokImagesUsageLogInfo([]byte(`{"data":[]}`)); info.Count != 0 {
		t.Fatalf("empty data info = %+v", info)
	}
}

// pending 期上游以 202 携带状态体返回;网关须按 200 透传而不是包装成错误。
// edits/extensions 缺省模型是 grok-imagine-video(1.5 系列不支持这两个操作)。
func TestGrokVideoPendingStatusAndPerOperationDefaultModel(t *testing.T) {
	var editModel string
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case r.Method == http.MethodPost && r.URL.Path == "/v1/videos/edits":
			body := readUpstreamRequestBody(r)
			editModel = gjson.GetBytes(body, "model").String()
			w.Header().Set("Content-Type", "application/json")
			_, _ = w.Write([]byte(`{"request_id":"video_pending1"}`))
		case r.Method == http.MethodGet && r.URL.Path == "/v1/videos/video_pending1":
			w.Header().Set("Content-Type", "application/json")
			w.WriteHeader(http.StatusAccepted)
			_, _ = w.Write([]byte(`{"status":"pending","progress":42}`))
		default:
			w.WriteHeader(http.StatusNotFound)
		}
	}))
	defer upstream.Close()

	account := &auth.Account{DBID: 1, UpstreamType: auth.UpstreamGrok, APIKey: "xai-test", BaseURL: upstream.URL}
	handler := newGrokMediaTestHandler(t, account)

	ctx, rec := newGrokMediaTestContext(t, http.MethodPost, "/v1/videos/edits", []byte(`{"prompt":"snow","video":{"url":"https://vidgen.x.ai/a.mp4"}}`))
	handler.VideosEdits(ctx)
	if rec.Code != http.StatusOK {
		t.Fatalf("edits create status = %d, body = %s", rec.Code, rec.Body.String())
	}
	if editModel != grokImagineVideoModel {
		t.Fatalf("edits default model = %q, want %q", editModel, grokImagineVideoModel)
	}

	ctx, rec = newGrokMediaTestContext(t, http.MethodGet, "/v1/videos/video_pending1", nil)
	ctx.Params = gin.Params{{Key: "request_id", Value: "video_pending1"}}
	handler.VideosStatus(ctx)
	if rec.Code != http.StatusOK {
		t.Fatalf("pending poll status = %d, body = %s", rec.Code, rec.Body.String())
	}
	if got := gjson.Get(rec.Body.String(), "status").String(); got != "pending" {
		t.Fatalf("pending body = %s", rec.Body.String())
	}
	if got := gjson.Get(rec.Body.String(), "progress").Int(); got != 42 {
		t.Fatalf("progress = %d, want 42", got)
	}
}

// CLI 身份头只发 CLI 网关:xAI 公开 API 收到 x-grok-client-* 会按 CLI 的
// Zero Data Retention 政策强制 output.upload_url,媒体请求 400。
func TestGrokMediaXAIProfileStripsCLIIdentityHeaders(t *testing.T) {
	var seen http.Header
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		seen = r.Header.Clone()
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"data":[{"url":"https://imgen.x.ai/a.jpeg"}]}`))
	}))
	defer upstream.Close()

	// API Key 账号 → xai profile
	apiKey := &auth.Account{DBID: 1, UpstreamType: auth.UpstreamGrok, APIKey: "xai-test", BaseURL: upstream.URL}
	handler := newGrokMediaTestHandler(t, apiKey)
	ctx, rec := newGrokMediaTestContext(t, http.MethodPost, "/v1/images/generations", []byte(`{"model":"grok-imagine-image","prompt":"x"}`))
	handler.ImagesGenerations(ctx)
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, body = %s", rec.Code, rec.Body.String())
	}
	for _, key := range []string{"x-grok-client-version", "x-grok-client-identifier", "x-grok-client-mode", "x-grok-model-override", "x-compaction-at"} {
		if seen.Get(key) != "" {
			t.Errorf("xai profile leaked CLI header %s=%q", key, seen.Get(key))
		}
	}
	if seen.Get("Authorization") != "Bearer xai-test" {
		t.Errorf("Authorization = %q", seen.Get("Authorization"))
	}

	// OAuth 账号 → cli profile 保留 CLI 头
	oauth := &auth.Account{DBID: 2, UpstreamType: auth.UpstreamGrok, AccessToken: "at", BaseURL: upstream.URL, PlanType: "supergrok_heavy"}
	handler = newGrokMediaTestHandler(t, oauth)
	ctx, rec = newGrokMediaTestContext(t, http.MethodPost, "/v1/images/generations", []byte(`{"model":"grok-imagine-image","prompt":"x"}`))
	handler.ImagesGenerations(ctx)
	if rec.Code != http.StatusOK {
		t.Fatalf("oauth status = %d, body = %s", rec.Code, rec.Body.String())
	}
	if seen.Get("x-grok-client-version") == "" || seen.Get("x-xai-token-auth") == "" {
		t.Error("cli profile must keep CLI identity headers")
	}
}
