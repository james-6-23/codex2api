package proxy

import (
	"context"
	"encoding/base64"
	"fmt"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/codex2api/auth"
	"github.com/codex2api/database"
	"github.com/codex2api/internal/imagestore"
	"github.com/gin-gonic/gin"
	"github.com/tidwall/gjson"
)

const tinyPNGBase64 = "iVBORw0KGgoAAAANSUhEUgAAAAEAAAABCAQAAAC1HAwCAAAAC0lEQVR42mP8/x8AAwMCAO+/p9sAAAAASUVORK5CYII="

func TestBuildImagesAPIResponseCloudURL(t *testing.T) {
	// 用本地 httptest 充当 S3 端点：PUT 返回 200 让上传成功；
	// presign 是离线签名，生成的 GET 直链会指向该测试服务器。
	fakeS3 := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("ETag", `"deadbeef"`)
		w.WriteHeader(http.StatusOK)
	}))
	t.Cleanup(fakeS3.Close)

	if err := imagestore.Configure(imagestore.Config{
		Backend:        imagestore.BackendS3,
		Endpoint:       fakeS3.URL,
		Region:         "us-east-1",
		Bucket:         "img-bucket",
		AccessKey:      "AKIAEXAMPLE",
		SecretKey:      "secretexample",
		ForcePathStyle: true,
	}); err != nil {
		t.Fatalf("configure s3: %v", err)
	}
	t.Cleanup(func() {
		_ = imagestore.Configure(imagestore.Config{Backend: imagestore.BackendLocal, LocalDir: t.TempDir()})
	})

	results := []imageCallResult{{Result: tinyPNGBase64, OutputFormat: "png", Model: "gpt-image-2"}}
	out, err := buildImagesAPIResponse(context.Background(), results, 1710000000, nil, results[0], "url", cloudImageURLOnly)
	if err != nil {
		t.Fatalf("buildImagesAPIResponse: %v", err)
	}
	url := gjson.GetBytes(out, "data.0.url").String()
	if !strings.HasPrefix(url, "http") || !strings.Contains(url, "X-Amz-Signature=") {
		t.Fatalf("expected presigned cloud url, got %q", url)
	}
	if strings.HasPrefix(url, "data:") {
		t.Fatalf("should not fall back to data url when S3 configured: %q", url)
	}
}

func TestBuildImagesAPIResponseLocalFallsBackToDataURL(t *testing.T) {
	if err := imagestore.Configure(imagestore.Config{Backend: imagestore.BackendLocal, LocalDir: t.TempDir()}); err != nil {
		t.Fatalf("configure local: %v", err)
	}
	results := []imageCallResult{{Result: tinyPNGBase64, OutputFormat: "png"}}
	out, err := buildImagesAPIResponse(context.Background(), results, 1710000000, nil, results[0], "url", nil)
	if err != nil {
		t.Fatalf("buildImagesAPIResponse: %v", err)
	}
	url := gjson.GetBytes(out, "data.0.url").String()
	if !strings.HasPrefix(url, "data:image/png;base64,") {
		t.Fatalf("expected data url fallback for local backend, got %q", url)
	}
}

func TestImageGalleryPersisterRecordsAssetAndJob(t *testing.T) {
	// 假 S3 端点：PUT 200 让上传成功；presign 离线签名生成直链。
	fakeS3 := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("ETag", `"deadbeef"`)
		w.WriteHeader(http.StatusOK)
	}))
	t.Cleanup(fakeS3.Close)
	if err := imagestore.Configure(imagestore.Config{
		Backend: imagestore.BackendS3, Endpoint: fakeS3.URL, Region: "us-east-1",
		Bucket: "img-bucket", AccessKey: "AKIAEXAMPLE", SecretKey: "secretexample", ForcePathStyle: true,
	}); err != nil {
		t.Fatalf("configure s3: %v", err)
	}
	t.Cleanup(func() {
		_ = imagestore.Configure(imagestore.Config{Backend: imagestore.BackendLocal, LocalDir: t.TempDir()})
	})

	db, err := database.New("sqlite", filepath.Join(t.TempDir(), "codex2api.db"))
	if err != nil {
		t.Fatalf("database.New: %v", err)
	}
	defer db.Close()

	ctx := context.Background()
	p := &imageGalleryPersister{
		h:        &Handler{db: db},
		prompt:   "draw a cat",
		apiKeyID: 7,
		model:    "gpt-image-2",
		start:    time.Now(),
	}
	image := imageCallResult{Result: tinyPNGBase64, OutputFormat: "png", Model: "gpt-image-2", Size: "1024x1024"}
	url, ok := p.buildURL(ctx, image, 0)
	if !ok || !strings.Contains(url, "X-Amz-Signature=") {
		t.Fatalf("buildURL ok=%v url=%q", ok, url)
	}
	p.finalize(ctx)

	// 应登记一条 asset，且 storage_path 为 s3:// ref。
	assets, err := db.ListImageAssets(ctx, 1, 10, 0)
	if err != nil {
		t.Fatalf("ListImageAssets: %v", err)
	}
	if assets.Total != 1 || len(assets.Assets) != 1 {
		t.Fatalf("expected 1 asset, got total=%d len=%d", assets.Total, len(assets.Assets))
	}
	asset := assets.Assets[0]
	if !imagestore.IsS3Ref(asset.StoragePath) {
		t.Fatalf("asset storage_path not s3 ref: %q", asset.StoragePath)
	}
	if asset.JobID == 0 {
		t.Fatalf("asset should be linked to a synthetic job, got job_id=0")
	}

	// synthetic job 应存在且标记为成功，携带 api_key_id。
	job, err := db.GetImageGenerationJob(ctx, asset.JobID)
	if err != nil {
		t.Fatalf("GetImageGenerationJob: %v", err)
	}
	if job.Status != database.ImageJobSucceeded {
		t.Fatalf("job status = %q, want succeeded", job.Status)
	}
	if job.APIKeyID != 7 {
		t.Fatalf("job api_key_id = %d, want 7", job.APIKeyID)
	}
}

func tinyPNGByteSize(t *testing.T) int {
	t.Helper()
	data, err := base64.StdEncoding.DecodeString(tinyPNGBase64)
	if err != nil {
		t.Fatalf("decode tiny png fixture: %v", err)
	}
	return len(data)
}

func TestBuildImagesResponsesRequestMatchesReferenceChain(t *testing.T) {
	tool := []byte(`{"type":"image_generation","action":"generate","model":"gpt-image-2","size":"1024x1024"}`)

	body := buildImagesResponsesRequest("draw a cat", nil, tool)

	if got := gjson.GetBytes(body, "model").String(); got != defaultImagesMainModel {
		t.Fatalf("responses model = %q, want %q", got, defaultImagesMainModel)
	}
	if got := gjson.GetBytes(body, "tool_choice.type").String(); got != "image_generation" {
		t.Fatalf("tool_choice.type = %q, want image_generation", got)
	}
	if got := gjson.GetBytes(body, "tools.0.type").String(); got != "image_generation" {
		t.Fatalf("tools.0.type = %q, want image_generation", got)
	}
	if got := gjson.GetBytes(body, "tools.0.model").String(); got != "gpt-image-2" {
		t.Fatalf("tools.0.model = %q, want gpt-image-2", got)
	}
	if got := gjson.GetBytes(body, "input.0.content.0.text").String(); got != "draw a cat" {
		t.Fatalf("prompt = %q, want draw a cat", got)
	}
}

func TestResponsesBodyHasImageGenerationTool(t *testing.T) {
	cases := []struct {
		name string
		body []byte
		want bool
	}{
		{"tool", []byte(`{"tools":[{"type":"image_generation","model":"gpt-image-2"}]}`), true},
		{"object_choice", []byte(`{"tool_choice":{"type":"image_generation"}}`), true},
		{"string_choice", []byte(`{"tool_choice":"image_generation"}`), true},
		{"function_tool", []byte(`{"tools":[{"type":"function","name":"lookup"}]}`), false},
		{"empty", []byte(`{}`), false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := responsesBodyHasImageGenerationTool(tc.body); got != tc.want {
				t.Fatalf("responsesBodyHasImageGenerationTool() = %v, want %v", got, tc.want)
			}
		})
	}
}

func TestResponsesBodyRequestsImageGenerationIgnoresDefaultInjectedTool(t *testing.T) {
	prepared, _ := PrepareResponsesBody([]byte(`{"model":"gpt-5.5","input":"hello"}`))
	if !responsesBodyHasImageGenerationTool(prepared) {
		t.Fatalf("test setup expected prepared body to include default image tool: %s", prepared)
	}
	if responsesBodyRequestsImageGeneration(prepared) {
		t.Fatalf("default injected image tool should not force HTTP image path: %s", prepared)
	}
}

func TestResponsesBodyRequestsImageGenerationDetectsExplicitIntent(t *testing.T) {
	cases := []struct {
		name string
		body []byte
	}{
		{"object_choice", []byte(`{"model":"gpt-5.5","tool_choice":{"type":"image_generation"}}`)},
		{"string_choice", []byte(`{"model":"gpt-5.5","tool_choice":"image_generation"}`)},
		{"image_model", []byte(`{"model":"gpt-image-2","prompt":"draw a cat"}`)},
		{"top_level_option", []byte(`{"model":"gpt-5.5","input":"draw a cat","size":"1024x1024"}`)},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if !responsesBodyRequestsImageGeneration(tc.body) {
				t.Fatalf("responsesBodyRequestsImageGeneration() = false, want true for %s", tc.body)
			}
		})
	}
}

func TestResponsesBodyHasNaturalImageGenerationIntent(t *testing.T) {
	cases := []struct {
		name string
		body []byte
		want bool
	}{
		{
			name: "chinese_direct_generation",
			body: []byte(`{"model":"gpt-5.5","input":"帮我生成一张赛博朋克风格的猫图片"}`),
			want: true,
		},
		{
			name: "prompt_compat_generation",
			body: []byte(`{"model":"gpt-5.5","prompt":"画一张水彩风的山景"}`),
			want: true,
		},
		{
			name: "meme_generation_not_table",
			body: []byte(`{"model":"gpt-5.5","input":"生成一张表情包"}`),
			want: true,
		},
		{
			name: "image_edit_text_part",
			body: []byte(`{"model":"gpt-5.5","input":[{"role":"user","content":[{"type":"input_text","text":"edit this image to make the background blue"}]}]}`),
			want: true,
		},
		{
			name: "plain_chat",
			body: []byte(`{"model":"gpt-5.5","input":"hello, explain this error"}`),
			want: false,
		},
		{
			name: "script_request",
			body: []byte(`{"model":"gpt-5.5","input":"帮我写一个生成图片的 Python 脚本"}`),
			want: false,
		},
		{
			name: "api_question",
			body: []byte(`{"model":"gpt-5.5","input":"介绍一下 image generation API 怎么调用"}`),
			want: false,
		},
		{
			name: "table_request",
			body: []byte(`{"model":"gpt-5.5","input":"生成一张表格对比这些方案"}`),
			want: false,
		},
		{
			name: "diagram_code_request",
			body: []byte(`{"model":"gpt-5.5","input":"生成一张架构图的 Mermaid 代码"}`),
			want: false,
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := responsesBodyHasNaturalImageGenerationIntent(tc.body); got != tc.want {
				t.Fatalf("responsesBodyHasNaturalImageGenerationIntent() = %v, want %v for %s", got, tc.want, tc.body)
			}
		})
	}
}

func TestRawResponsesBodyShouldForceHTTPForImageGeneration(t *testing.T) {
	cases := []struct {
		name string
		body []byte
		want bool
	}{
		{"explicit_tool_choice", []byte(`{"model":"gpt-5.5","tool_choice":{"type":"image_generation"}}`), true},
		{"natural_language_generation", []byte(`{"model":"gpt-5.5","input":"生成一张未来城市海报"}`), true},
		{"image_only_model", []byte(`{"model":"gpt-image-2","prompt":"draw a cat"}`), true},
		{"top_level_option", []byte(`{"model":"gpt-5.5","input":"hi","size":"1024x1024"}`), true},
		{"plain_request", []byte(`{"model":"gpt-5.5","input":"hello"}`), false},
		{"image_generation_code_request", []byte(`{"model":"gpt-5.5","input":"帮我写一个生成图片的 Python 脚本"}`), false},
		// issue #304: 注入的 image_generation 工具但无 tool_choice / 无自然语言意图，
		// 不应因工具单纯存在而强制 HTTP，普通请求继续走 WS。
		{"injected_tool_without_intent", []byte(`{"model":"gpt-5.5","input":"hello","tools":[{"type":"image_generation"}]}`), false},
		{"injected_tool_plain_chat", []byte(`{"model":"gpt-5.5","input":"解释一下这段报错","tools":[{"type":"image_generation","model":"gpt-image-2"},{"type":"function","name":"lookup"}]}`), false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := rawResponsesBodyShouldForceHTTPForImageGeneration(tc.body); got != tc.want {
				t.Fatalf("rawResponsesBodyShouldForceHTTPForImageGeneration() = %v, want %v for %s", got, tc.want, tc.body)
			}
		})
	}
}

func TestStripResponsesImageGenerationToolRemovesInjectedTool(t *testing.T) {
	// PrepareResponsesBody 默认给普通请求注入 image_generation 工具 + 桥接 instructions；WS 路径需全部剥离。
	prepared, _ := PrepareResponsesBody([]byte(`{"model":"gpt-5.5","input":"hello"}`))
	if !responsesBodyHasImageGenerationTool(prepared) {
		t.Fatalf("setup: prepared body should contain injected image tool: %s", prepared)
	}
	stripped := stripResponsesImageGenerationTool(prepared)
	if responsesBodyHasImageGenerationTool(stripped) {
		t.Fatalf("stripped body should not contain image tool: %s", stripped)
	}
	if strings.Contains(string(stripped), "image_generation") {
		t.Fatalf("stripped body should not mention image_generation anywhere: %s", stripped)
	}
}

func TestStripResponsesImageGenerationToolPreservesUserInstructions(t *testing.T) {
	prepared, _ := PrepareResponsesBody([]byte(`{"model":"gpt-5.5","input":"hello","instructions":"You are a helpful assistant."}`))
	stripped := stripResponsesImageGenerationTool(prepared)
	instructions := gjson.GetBytes(stripped, "instructions").String()
	if !strings.Contains(instructions, "You are a helpful assistant.") {
		t.Fatalf("user instructions should be preserved: %s", stripped)
	}
	if strings.Contains(instructions, codexImageGenerationBridgeMarker) {
		t.Fatalf("bridge instructions should be removed: %s", stripped)
	}
}

func TestStripResponsesImageGenerationToolKeepsOtherTools(t *testing.T) {
	body := []byte(`{"tools":[{"type":"function","name":"lookup"},{"type":"image_generation","model":"gpt-image-2"}],"tool_choice":{"type":"image_generation"}}`)
	stripped := stripResponsesImageGenerationTool(body)
	if responsesBodyHasImageGenerationTool(stripped) {
		t.Fatalf("image tool/choice should be removed: %s", stripped)
	}
	tools := gjson.GetBytes(stripped, "tools").Array()
	if len(tools) != 1 || tools[0].Get("type").String() != "function" {
		t.Fatalf("function tool should be preserved: %s", stripped)
	}
	if gjson.GetBytes(stripped, "tool_choice").Exists() {
		t.Fatalf("image tool_choice should be removed: %s", stripped)
	}
}

func TestStripResponsesImageGenerationToolNoopWithoutImageTool(t *testing.T) {
	body := []byte(`{"tools":[{"type":"function","name":"lookup"}]}`)
	stripped := stripResponsesImageGenerationTool(body)
	if gjson.GetBytes(stripped, "tools.0.type").String() != "function" {
		t.Fatalf("non-image tools should be untouched: %s", stripped)
	}
}

func TestTranslateRequestDoesNotFlagPlainChatAsImageGeneration(t *testing.T) {
	// Chat 入口用 codexBody 判定，TranslateRequest 不应注入图片工具，否则普通对话会被误判强制 HTTP。
	codexBody, err := TranslateRequest([]byte(`{"model":"gpt-5.5","messages":[{"role":"user","content":"hello"}]}`))
	if err != nil {
		t.Fatalf("TranslateRequest: %v", err)
	}
	if rawResponsesBodyShouldForceHTTPForImageGeneration(codexBody) {
		t.Fatalf("plain chat request should not be flagged as image generation: %s", codexBody)
	}
}

func TestNextImageAccountPrefersPlusOrHigherPlan(t *testing.T) {
	store := auth.NewStore(nil, nil, &database.SystemSettings{MaxConcurrency: 2, TestConcurrency: 1, TestModel: "gpt-5.4"})
	store.AddAccount(&auth.Account{DBID: 1, AccessToken: "free-token", PlanType: "free"})
	store.AddAccount(&auth.Account{DBID: 2, AccessToken: "plus-token", PlanType: "plus"})
	handler := &Handler{store: store}

	account, _ := handler.nextImageAccount(0, nil, "")
	if account == nil {
		t.Fatal("nextImageAccount returned nil")
	}
	defer store.Release(account)

	if account.DBID != 2 {
		t.Fatalf("nextImageAccount picked account %d, want plus account 2", account.DBID)
	}
}

func TestNextImageAccountFallsBackToFreeWhenNoPaidAccountAvailable(t *testing.T) {
	store := auth.NewStore(nil, nil, &database.SystemSettings{MaxConcurrency: 2, TestConcurrency: 1, TestModel: "gpt-5.4"})
	store.AddAccount(&auth.Account{DBID: 1, AccessToken: "free-token", PlanType: "free"})
	handler := &Handler{store: store}

	account, _ := handler.nextImageAccount(0, nil, "")
	if account == nil {
		t.Fatal("nextImageAccount returned nil")
	}
	defer store.Release(account)

	if account.DBID != 1 {
		t.Fatalf("nextImageAccount picked account %d, want fallback free account 1", account.DBID)
	}
}

func TestAppendImageStyleToPrompt(t *testing.T) {
	got := AppendImageStyleToPrompt("draw a cat", "cinematic sticker")
	if !strings.Contains(got, "draw a cat") || !strings.Contains(got, "Style guidance: cinematic sticker") {
		t.Fatalf("styled prompt = %q", got)
	}
	if got := AppendImageStyleToPrompt("draw a cat", " "); got != "draw a cat" {
		t.Fatalf("unstyled prompt = %q, want draw a cat", got)
	}
}

func TestNormalizeImageToolModelAliases(t *testing.T) {
	tests := []struct {
		name     string
		model    string
		want     string
		wantSize string
	}{
		{name: "default model", model: "gpt-image-2", want: "gpt-image-2", wantSize: defaultImages1KSize},
		{name: "2k alias", model: "gpt-image-2-2k", want: "gpt-image-2", wantSize: defaultImages2KSize},
		{name: "4k alias", model: "gpt-image-2-4k", want: "gpt-image-2", wantSize: defaultImages4KSize},
		{name: "other image model", model: "gpt-image-1.5", want: "gpt-image-1.5", wantSize: ""},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			got, gotSize := normalizeImageToolModel(test.model)
			if got != test.want || gotSize != test.wantSize {
				t.Fatalf("normalizeImageToolModel(%q) = (%q, %q), want (%q, %q)", test.model, got, gotSize, test.want, test.wantSize)
			}
		})
	}
}

func TestNormalizeImageToolModelForPromptInfersAspect(t *testing.T) {
	tests := []struct {
		name     string
		model    string
		prompt   string
		want     string
		wantSize string
	}{
		{
			name:     "1k landscape prompt",
			model:    defaultImagesToolModel,
			prompt:   "desktop wallpaper, wide cinematic city",
			want:     defaultImagesToolModel,
			wantSize: defaultImages1KLandscapeSize,
		},
		{
			name:     "2k portrait prompt",
			model:    imageModel2KAlias,
			prompt:   "mobile wallpaper portrait neon cat",
			want:     defaultImagesToolModel,
			wantSize: defaultImages2KPortraitSize,
		},
		{
			name:     "4k square prompt",
			model:    imageModel4KAlias,
			prompt:   "square app icon logo",
			want:     defaultImagesToolModel,
			wantSize: defaultImages4KSquareSize,
		},
		{
			name:     "4k no prompt keeps default",
			model:    imageModel4KAlias,
			prompt:   "a detailed fantasy city",
			want:     defaultImagesToolModel,
			wantSize: defaultImages4KSize,
		},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			got, gotSize := normalizeImageToolModelForPrompt(test.model, test.prompt)
			if got != test.want || gotSize != test.wantSize {
				t.Fatalf("normalizeImageToolModelForPrompt(%q, %q) = (%q, %q), want (%q, %q)", test.model, test.prompt, got, gotSize, test.want, test.wantSize)
			}
		})
	}
}

func TestSetDefaultImageToolSizePreservesExplicitSize(t *testing.T) {
	tool := []byte(`{"type":"image_generation","model":"gpt-image-2","size":"1536x1024"}`)

	got := setDefaultImageToolSize(tool, defaultImages4KSize)

	if size := gjson.GetBytes(got, "size").String(); size != "1536x1024" {
		t.Fatalf("size = %q, want explicit size", size)
	}
}

func TestValidateGPTImage2Size(t *testing.T) {
	tests := []struct {
		name    string
		size    string
		wantErr bool
	}{
		{name: "auto", size: "auto"},
		{name: "1k", size: defaultImages1KSize},
		{name: "2k square", size: defaultImages2KSize},
		{name: "4k landscape", size: defaultImages4KSize},
		{name: "4k portrait", size: "2160x3840"},
		{name: "too many pixels", size: "5000x5000", wantErr: true},
		{name: "too wide", size: "4096x1024", wantErr: true},
		{name: "not multiple of 16", size: "1025x1024", wantErr: true},
		{name: "bad format", size: "1024*1024", wantErr: true},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			err := validateGPTImage2Size(test.size)
			if test.wantErr && err == nil {
				t.Fatalf("validateGPTImage2Size(%q) expected error", test.size)
			}
			if !test.wantErr && err != nil {
				t.Fatalf("validateGPTImage2Size(%q) unexpected error: %v", test.size, err)
			}
		})
	}
}

func TestValidateResponsesImageGenerationSizes(t *testing.T) {
	valid := []byte(`{"tools":[{"type":"image_generation","model":"gpt-image-2","size":"3840x2160"}]}`)
	if err := validateResponsesImageGenerationSizes(valid); err != nil {
		t.Fatalf("valid image_generation size returned error: %v", err)
	}

	invalid := []byte(`{"tools":[{"type":"image_generation","model":"gpt-image-2","size":"5000x5000"}]}`)
	if err := validateResponsesImageGenerationSizes(invalid); err == nil {
		t.Fatal("expected invalid image_generation size error")
	}

	nonString := []byte(`{"tools":[{"type":"image_generation","model":"gpt-image-2","size":1024}]}`)
	if err := validateResponsesImageGenerationSizes(nonString); err == nil {
		t.Fatal("expected non-string image_generation size error")
	}

	otherModel := []byte(`{"tools":[{"type":"image_generation","model":"gpt-image-1.5","size":"5000x5000"}]}`)
	if err := validateResponsesImageGenerationSizes(otherModel); err != nil {
		t.Fatalf("non gpt-image-2 size should be ignored, got %v", err)
	}
}

func TestBuildImagesResponsesRequestIncludesEditImages(t *testing.T) {
	tool := []byte(`{"type":"image_generation","action":"edit","model":"gpt-image-2"}`)

	body := buildImagesResponsesRequest("replace background", []string{"https://example.com/source.png"}, tool)

	if got := gjson.GetBytes(body, "tools.0.action").String(); got != "edit" {
		t.Fatalf("tools.0.action = %q, want edit", got)
	}
	if got := gjson.GetBytes(body, "input.0.content.1.type").String(); got != "input_image" {
		t.Fatalf("input image type = %q, want input_image", got)
	}
	if got := gjson.GetBytes(body, "input.0.content.1.image_url").String(); got != "https://example.com/source.png" {
		t.Fatalf("input image URL = %q", got)
	}
}

func TestCollectImagesResponseBuildsOpenAIImagePayload(t *testing.T) {
	upstream := `data: {"type":"response.completed","response":{"created_at":1710000000,"usage":{"input_tokens":5,"output_tokens":9},"tool_usage":{"image_gen":{"images":1,"input_tokens":34,"output_tokens":1756}},"tools":[{"type":"image_generation","model":"gpt-image-2","output_format":"png","quality":"high","size":"1024x1024"}],"output":[{"type":"image_generation_call","result":"` + tinyPNGBase64 + `","revised_prompt":"draw a cat","output_format":"png"}]}}` + "\n\n"

	out, usage, imageCount, imageLogInfo, err := collectImagesResponse(context.Background(), strings.NewReader(upstream), "b64_json", "gpt-image-2", nil)
	if err != nil {
		t.Fatalf("collectImagesResponse returned error: %v", err)
	}
	if imageCount != 1 {
		t.Fatalf("imageCount = %d, want 1", imageCount)
	}
	if imageLogInfo.Count != 1 || imageLogInfo.Width != 1 || imageLogInfo.Height != 1 || imageLogInfo.Bytes != tinyPNGByteSize(t) {
		t.Fatalf("imageLogInfo = %#v, want count=1 size=1x1 bytes=%d", imageLogInfo, tinyPNGByteSize(t))
	}
	if usage == nil || usage.InputTokens != 34 || usage.OutputTokens != 1756 {
		t.Fatalf("usage = %#v, want image usage input=34 output=1756", usage)
	}
	if got := gjson.GetBytes(out, "data.0.b64_json").String(); got != tinyPNGBase64 {
		t.Fatalf("b64_json = %q, want tiny PNG", got)
	}
	if got := gjson.GetBytes(out, "data.0.bytes").Int(); got != int64(tinyPNGByteSize(t)) {
		t.Fatalf("bytes = %d, want %d", got, tinyPNGByteSize(t))
	}
	if got := gjson.GetBytes(out, "data.0.width").Int(); got != 1 {
		t.Fatalf("width = %d, want 1", got)
	}
	if got := gjson.GetBytes(out, "data.0.height").Int(); got != 1 {
		t.Fatalf("height = %d, want 1", got)
	}
	if got := gjson.GetBytes(out, "model").String(); got != "gpt-image-2" {
		t.Fatalf("model = %q, want gpt-image-2", got)
	}
	if got := gjson.GetBytes(out, "usage.images").Int(); got != 1 {
		t.Fatalf("usage.images = %d, want 1", got)
	}
}

func TestCollectImagesResponseUsesUpstreamFailureMessage(t *testing.T) {
	upstream := `data: {"type":"response.failed","response":{"error":{"code":"server_error","message":"An error occurred while processing your request. Please include the request ID req-123."}}}` + "\n\n"

	_, _, _, _, err := collectImagesResponse(context.Background(), strings.NewReader(upstream), "b64_json", "gpt-image-2", nil)
	if err == nil {
		t.Fatal("collectImagesResponse returned nil error")
	}
	if got := err.Error(); !strings.Contains(got, "server_error") || !strings.Contains(got, "req-123") {
		t.Fatalf("error = %q, want upstream code and request id", got)
	}
}

func TestBuildImageErrorUsageLogRecordsFailure(t *testing.T) {
	account := &auth.Account{DBID: 42, AccessToken: "token", PlanType: "plus"}
	readErr := fmt.Errorf("upstream image generation failed: server_error")
	usage := &UsageInfo{InputTokens: 12, OutputTokens: 3, TotalTokens: 15, PromptTokens: 12, CompletionTokens: 3}
	imageLogInfo := imageUsageLogInfo{Count: 1, Width: 1024, Height: 1024, Bytes: 2048, Format: "png", Size: "1024x1024"}

	logInput := buildImageErrorUsageLog(account, "/v1/images/generations", "gpt-image-2", "gpt-image-2", false, 1500, 1, true, readErr, usage, imageLogInfo)

	if logInput.AccountID != 42 {
		t.Fatalf("AccountID = %d, want 42", logInput.AccountID)
	}
	if logInput.StatusCode != http.StatusBadGateway {
		t.Fatalf("StatusCode = %d, want %d", logInput.StatusCode, http.StatusBadGateway)
	}
	if logInput.DurationMs != 1500 {
		t.Fatalf("DurationMs = %d, want 1500", logInput.DurationMs)
	}
	if !logInput.IsRetryAttempt || logInput.AttemptIndex != 2 {
		t.Fatalf("retry fields = (%v, %d), want (true, 2)", logInput.IsRetryAttempt, logInput.AttemptIndex)
	}
	if logInput.ErrorMessage == "" {
		t.Fatal("ErrorMessage is empty, want upstream failure detail")
	}
	if logInput.PromptTokens != 12 || logInput.CompletionTokens != 3 || logInput.TotalTokens != 15 {
		t.Fatalf("token fields = (%d, %d, %d), want (12, 3, 15)", logInput.PromptTokens, logInput.CompletionTokens, logInput.TotalTokens)
	}
	if logInput.ImageCount != 1 || logInput.ImageWidth != 1024 || logInput.ImageFormat != "png" {
		t.Fatalf("image fields = %#v, want count=1 width=1024 format=png", logInput)
	}
}

func TestStartImageStreamKeepaliveStopsWhenWriterFails(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	var mu sync.Mutex
	writes := 0
	wrote := make(chan struct{}, 1)
	stop := startImageStreamKeepalive(ctx, time.Millisecond, func() bool {
		mu.Lock()
		writes++
		mu.Unlock()
		select {
		case wrote <- struct{}{}:
		default:
		}
		return false
	})
	defer stop()

	select {
	case <-wrote:
	case <-time.After(100 * time.Millisecond):
		t.Fatal("keepalive did not write")
	}
	time.Sleep(10 * time.Millisecond)
	mu.Lock()
	finalWrites := writes
	mu.Unlock()
	if finalWrites != 1 {
		t.Fatalf("keepalive kept writing after writer failure: got %d writes, want 1", finalWrites)
	}
}

func TestStreamImagesResponseSendsConnectedComment(t *testing.T) {
	gin.SetMode(gin.TestMode)
	upstream := `data: {"type":"response.completed","response":{"created_at":1710000000,"usage":{"input_tokens":5,"output_tokens":9},"tool_usage":{"image_gen":{"images":1,"input_tokens":34,"output_tokens":1756}},"tools":[{"type":"image_generation","model":"gpt-image-2","output_format":"png","quality":"high","size":"1024x1024"}],"output":[{"type":"image_generation_call","result":"` + tinyPNGBase64 + `","revised_prompt":"draw a cat","output_format":"png"}]}}` + "\n\n"
	recorder := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(recorder)
	c.Request = httptest.NewRequest("POST", "/v1/images/generations", nil)
	handler := &Handler{}

	usage, imageCount, _, imageLogInfo, err := handler.streamImagesResponse(c, strings.NewReader(upstream), "b64_json", "image_generation", "gpt-image-2", time.Now())

	if err != nil {
		t.Fatalf("streamImagesResponse returned error: %v", err)
	}
	if imageCount != 1 {
		t.Fatalf("imageCount = %d, want 1", imageCount)
	}
	if usage == nil || usage.InputTokens != 34 || usage.OutputTokens != 1756 {
		t.Fatalf("usage = %#v, want image usage input=34 output=1756", usage)
	}
	if imageLogInfo.Count != 1 || imageLogInfo.Width != 1 || imageLogInfo.Height != 1 {
		t.Fatalf("imageLogInfo = %#v, want one 1x1 image", imageLogInfo)
	}
	if got := recorder.Header().Get("Content-Type"); got != "text/event-stream" {
		t.Fatalf("Content-Type = %q, want text/event-stream", got)
	}
	body := recorder.Body.String()
	if !strings.HasPrefix(body, imageStreamConnectedComment) {
		t.Fatalf("stream body should start with connected comment, got %q", body)
	}
	if !strings.Contains(body, "event: image_generation.completed\n") {
		t.Fatalf("stream body missing completed event: %q", body)
	}
}
