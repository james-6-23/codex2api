package proxy

import (
	"bytes"
	"context"
	"fmt"
	"mime/multipart"
	"net/http"
	"net/http/httptest"
	"slices"
	"strings"
	"testing"

	"github.com/codex2api/auth"
	"github.com/codex2api/config"
	"github.com/codex2api/database"
	"github.com/gin-gonic/gin"
	"github.com/tidwall/gjson"
)

func TestImagesIngressToUpstream(t *testing.T) {
	gin.SetMode(gin.TestMode)
	previousRuntime, previousResin := CurrentRuntimeSettings(), resinCfg.Load()
	t.Cleanup(func() { ApplyRuntimeSettings(previousRuntime); resinCfg.Store(previousResin) })
	runtime := previousRuntime
	runtime.ContinuousRetryPolicy = database.ContinuousRetryPolicy{}
	ApplyRuntimeSettings(runtime)
	t.Setenv(imagesMainModelEnv, "gpt-5.5")
	observed := make(chan []byte, 1)
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		forwarded := readUpstreamRequestBody(r)
		w.Header().Set("Content-Type", "text/event-stream")
		// The Codex backend rejects explicit reasoning effort on image turns,
		// independently of the image tool's quality setting.
		if effort := gjson.GetBytes(forwarded, "reasoning.effort"); effort.Exists() {
			w.Header().Set("Content-Type", "application/json")
			w.WriteHeader(http.StatusBadRequest)
			fmt.Fprintf(w, `{"error":{"type":"invalid_request_error","code":"invalid_value","message":"Complexity '%s' is not supported for this image model."}}`, effort.String())
			return
		}
		observed <- forwarded
		fmt.Fprint(w, `data: {"type":"response.completed","response":{"tool_usage":{"image_gen":{"input_tokens":1550,"input_tokens_details":{"text_tokens":29,"image_tokens":1521},"output_tokens":515,"output_tokens_details":{"image_tokens":515}}},"tools":[{"type":"image_generation","model":"gpt-image-2-codex"}],"output":[{"type":"image_generation_call","result":"`+tinyPNGBase64+`","output_format":"png"}]}}`+"\n\n")
	}))
	defer upstream.Close()
	SetResinConfig(&ResinConfig{BaseURL: upstream.URL, PlatformName: "image25-test"})
	store := auth.NewStore(nil, nil, &database.SystemSettings{MaxConcurrency: 1, TestConcurrency: 1, MaxRetries: 0})
	defer store.Stop()
	store.AddAccount(&auth.Account{DBID: 1, AccessToken: "test-token", AccountID: "image-test-account", PlanType: "plus"})
	handler := NewHandler(store, nil, &config.Config{AllowAnonymousV1: true}, nil)
	for _, tc := range []struct {
		model, quality, mode, driver string
		stream                       bool
	}{
		{"gpt-image-2", "auto", "generate", "gpt-6.1-terra", false},
		{"gpt-image-2", "medium", "json", "gpt-6.1-terra", false},
		{"gpt-image-2.5-flare", "xhigh", "generate", "gpt-5.6-sol", false},
		{"gpt-image-2.5-sunburst", "max", "generate", "gpt-5.6-terra", true},
		{"gpt-image-2.5-sunburst-2026-09-08", "max", "json", "gpt-5.6-sol", false},
		{"gpt-image-2.5-flare", "xhigh", "multipart", "gpt-5.6-terra", true},
		{"gpt-image-2.5-flare", "max", "studio_generate", "gpt-5.6-sol", false},
		{"gpt-image-2.5-sunburst", "xhigh", "studio_edit", "gpt-5.6-terra", false},
	} {
		t.Run(tc.mode+tc.model, func(t *testing.T) {
			// One handler observes changes made between requests without restarting.
			UpdateRuntimeSettings(func(current RuntimeSettings) RuntimeSettings {
				current.CodexImagesMainModel = tc.driver
				return current
			})
			path := "/v1/images/generations"
			edit := tc.mode != "generate" && tc.mode != "studio_generate"
			if edit {
				path = "/v1/images/edits"
			}
			body := fmt.Sprintf(`{"model":%q,"prompt":"draw a red cup","quality":%q,"size":"1024x1024","stream":%t,"images":[{"image_url":"data:image/png;base64,%s"}]}`, tc.model, tc.quality, tc.stream, tinyPNGBase64)
			req := httptest.NewRequest(http.MethodPost, path, strings.NewReader(body))
			req.Header.Set("Content-Type", "application/json")
			if tc.mode == "multipart" {
				var buffer bytes.Buffer
				writer := multipart.NewWriter(&buffer)
				for key, value := range map[string]string{"model": tc.model, "prompt": "draw a red cup", "quality": tc.quality, "stream": "true", "size": "1024x1024"} {
					if err := writer.WriteField(key, value); err != nil {
						t.Fatal(err)
					}
				}
				file, err := writer.CreateFormFile("image", "input.png")
				if err != nil {
					t.Fatal(err)
				}
				png, _ := decodeImageBase64(tinyPNGBase64)
				if _, err = file.Write(png); err != nil {
					t.Fatal(err)
				}
				if err = writer.Close(); err != nil {
					t.Fatal(err)
				}
				req = httptest.NewRequest(http.MethodPost, path, &buffer)
				req.Header.Set("Content-Type", writer.FormDataContentType())
			}
			recorder := httptest.NewRecorder()
			c, _ := gin.CreateTestContext(recorder)
			c.Request = req
			if strings.HasPrefix(tc.mode, "studio_") {
				generate := handler.GenerateImageOnceForAdmin
				if edit {
					generate = handler.GenerateImageEditForAdmin
				}
				result, status, err := generate(context.Background(), []byte(body), nil, false)
				if err != nil {
					t.Fatal(err)
				}
				recorder.WriteHeader(status)
				recorder.Write(result)
			} else if tc.mode == "generate" {
				handler.ImagesGenerations(c)
			} else {
				handler.ImagesEdits(c)
			}
			if recorder.Code != http.StatusOK || !strings.Contains(recorder.Body.String(), tinyPNGBase64) {
				t.Fatalf("image request failed: %d %s", recorder.Code, recorder.Body.String())
			}
			forwarded := <-observed
			if gjson.GetBytes(forwarded, "reasoning.summary").String() != "auto" || gjson.GetBytes(forwarded, "include.0").String() != "reasoning.encrypted_content" {
				t.Fatalf("reasoning metadata changed: %s", forwarded)
			}
			if gjson.GetBytes(forwarded, "model").String() != tc.driver || gjson.GetBytes(forwarded, "tools.0.model").String() != tc.model || gjson.GetBytes(forwarded, "tools.0.quality").String() != tc.quality {
				t.Fatalf("outbound model/quality changed: %s", forwarded)
			}
			if edit && gjson.GetBytes(forwarded, "input.0.content.1.type").String() != "input_image" {
				t.Fatalf("edit lost input: %s", forwarded)
			}
		})
	}
}

func TestGPTImage25UsageAndResponsesModel(t *testing.T) {
	usage := extractUsageFromResult(gjson.Parse(`{"input_tokens":1550,"input_tokens_details":{"image_tokens":1521},"output_tokens":515,"output_tokens_details":{"image_tokens":515}}`))
	log := buildImageErrorUsageLog(&auth.Account{DBID: 1}, "/v1/images/edits", "gpt-image-2.5-flare", "", false, 0, 0, false, fmt.Errorf("interrupted"), usage, imageUsageLogInfo{})
	if log.ImageInputTokens != 1521 || log.ImageOutputTokens != 515 {
		t.Fatalf("usage detail lost: %+v", log)
	}
	if cost := database.UsageLogBilledCost(log); cost < 0.0277629 || cost > 0.0277631 {
		t.Fatalf("cost = %v", cost)
	}
	body, _ := PrepareResponsesBody([]byte(`{"model":"gpt-5.6-sol","input":"draw","tools":[{"type":"image_generation","model":"gpt-image-2.5-sunburst-2026-09-08","quality":"max"}]}`))
	if gjson.GetBytes(body, "model").String() != "gpt-5.6-sol" || gjson.GetBytes(body, "tools.0.model").String() != "gpt-image-2.5-sunburst-2026-09-08" || gjson.GetBytes(body, "tools.0.quality").String() != "max" {
		t.Fatalf("Responses fields changed: %s", body)
	}
	for _, model := range []string{"gpt-image-2.5-flare", "gpt-image-2.5-sunburst-4k"} {
		if !slices.Contains(SupportedModelIDs(context.Background(), nil), model) {
			t.Fatalf("model not exposed: %s", model)
		}
	}
}

func TestGPTImage25OfficialPricingIgnoresBatch(t *testing.T) {
	body := []byte(`
<section data-pricing-tier="standard"><table><thead><tr><th>Model</th><th>Modality</th><th>Input</th><th>Cached input</th><th>Output</th></tr></thead><tbody>
<tr><td>gpt-image-2.5-flare</td><td>Image</td><td>$8.00</td><td>$2.00</td><td>$30.00</td></tr>
<tr><td>gpt-image-2.5-flare</td><td>Text</td><td>$5.00</td><td>$1.25</td><td>-</td></tr>
</tbody></table></section>
<section data-pricing-tier="batch"><table><thead><tr><th>Model</th><th>Modality</th><th>Input</th><th>Cached input</th><th>Output</th></tr></thead><tbody>
<tr><td>gpt-image-2.5-flare</td><td>Image</td><td>$4.00</td><td>$1.00</td><td>$15.00</td></tr>
</tbody></table></section>
`)
	prices, err := ParseOpenAIOfficialPricingHTML(body)
	if err != nil {
		t.Fatal(err)
	}
	p := prices["gpt-image-2.5-flare"]
	if p.Input != 5 || p.CachedInput != 1.25 || p.ImageInput != 8 || p.CachedImageInput != 2 || p.Output != 30 {
		t.Fatalf("wrong image pricing: %+v", p)
	}
}
