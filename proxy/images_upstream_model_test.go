package proxy

import (
	"context"
	"strings"
	"testing"

	"github.com/codex2api/database"
)

// issue #882: 上游渠道（如 New API）执行了模型映射时，上游自报的 response.model
// 是唯一能拿到实际模型的信号。观测必须写入 upstream_response_model，并在本地
// 未记录映射时回填 effective_model，使统计主列表按实际模型聚合。
func TestApplyImageUpstreamModelObservationMappedImageModel(t *testing.T) {
	input := &database.UsageLogInput{Model: "gpt-image-2"}
	info := imageUsageLogInfo{Count: 1, ResponseModel: "gpt-image-2.6-sunburst"}

	applyImageUpstreamModelObservation(input, info, "gpt-image-2.6-sunburst", "gpt-image-2")

	if input.UpstreamResponseModel != "gpt-image-2.6-sunburst" {
		t.Fatalf("upstream_response_model = %q, want gpt-image-2.6-sunburst", input.UpstreamResponseModel)
	}
	if input.UpstreamModelMismatch == nil || *input.UpstreamModelMismatch {
		t.Fatalf("upstream_model_mismatch = %v, want false (实际发送与自报一致)", input.UpstreamModelMismatch)
	}
	if input.EffectiveModel != "gpt-image-2.6-sunburst" {
		t.Fatalf("effective_model = %q, want gpt-image-2.6-sunburst", input.EffectiveModel)
	}
	if input.Model != "gpt-image-2" {
		t.Fatalf("model = %q, want 请求模型 gpt-image-2 双记", input.Model)
	}
}

// 上游自报非图片族模型（Codex /responses 容器模型）不得推导 effective_model，
// 否则统计会被 gpt-5.x 污染；审计字段仍要记录。
func TestApplyImageUpstreamModelObservationNonImageSelfReport(t *testing.T) {
	input := &database.UsageLogInput{Model: "gpt-image-2"}
	mismatch := false
	info := imageUsageLogInfo{Count: 1, ResponseModel: "gpt-5.3-codex"}

	applyImageUpstreamModelObservation(input, info, "gpt-image-2", "gpt-image-2")

	if input.UpstreamResponseModel != "gpt-5.3-codex" {
		t.Fatalf("upstream_response_model = %q, want gpt-5.3-codex", input.UpstreamResponseModel)
	}
	if input.UpstreamModelMismatch == nil || !*input.UpstreamModelMismatch {
		t.Fatalf("upstream_model_mismatch = %v, want true", input.UpstreamModelMismatch)
	}
	if input.EffectiveModel != "" {
		t.Fatalf("effective_model = %q, want empty", input.EffectiveModel)
	}
	_ = mismatch
}

// 本地映射已记录 effective_model 时不得覆盖；上游未自报时不得改动任何字段。
func TestApplyImageUpstreamModelObservationRespectsLocalMappingAndSilence(t *testing.T) {
	mapped := &database.UsageLogInput{Model: "gpt-image-2", EffectiveModel: "gpt-image-2.6-sunburst"}
	applyImageUpstreamModelObservation(mapped, imageUsageLogInfo{Count: 1, ResponseModel: "gpt-image-2.6-sunburst"}, "gpt-image-2.6-sunburst", "gpt-image-2")
	if mapped.EffectiveModel != "gpt-image-2.6-sunburst" || mapped.UpstreamResponseModel != "gpt-image-2.6-sunburst" {
		t.Fatalf("本地映射被覆盖: effective=%q upstream=%q", mapped.EffectiveModel, mapped.UpstreamResponseModel)
	}

	silent := &database.UsageLogInput{Model: "gpt-image-2"}
	applyImageUpstreamModelObservation(silent, imageUsageLogInfo{Count: 1}, "gpt-image-2", "gpt-image-2")
	if silent.UpstreamResponseModel != "" || silent.UpstreamModelMismatch != nil || silent.EffectiveModel != "" {
		t.Fatalf("未自报时字段被改动: %+v", silent)
	}
}

// 生命周期帧的 response.model 必须进入 imageCallResult，作为观测来源。
func TestExtractImageMetaFromLifecycleEventCapturesResponseModel(t *testing.T) {
	payload := []byte(`{"type":"response.created","response":{"model":"gpt-image-2.6-sunburst","tools":[{"type":"image_generation","model":"gpt-image-2"}],"created_at":1700000000}}`)
	meta, createdAt, ok := extractImageMetaFromLifecycleEvent(payload)
	if !ok {
		t.Fatal("expected meta from lifecycle event")
	}
	if createdAt != 1700000000 {
		t.Fatalf("created_at = %d", createdAt)
	}
	if meta.Model != "gpt-image-2" {
		t.Fatalf("tools.0.model 回显 = %q, want gpt-image-2", meta.Model)
	}
	if meta.ResponseModel != "gpt-image-2.6-sunburst" {
		t.Fatalf("response.model = %q, want gpt-image-2.6-sunburst", meta.ResponseModel)
	}
}

// mergeImageMeta 必须沿 firstMeta → 结果图片 → imageUsageLogInfo 传递自报模型，
// 流式与非流式路径都依赖这条合并链。
func TestMergeImageMetaAndUsageLogInfoCarryResponseModel(t *testing.T) {
	target := imageCallResult{Model: "gpt-image-2"}
	mergeImageMeta(&target, imageCallResult{ResponseModel: "gpt-image-2.6-sunburst"})
	if target.ResponseModel != "gpt-image-2.6-sunburst" {
		t.Fatalf("mergeImageMeta 未传递 ResponseModel: %+v", target)
	}

	info := imageUsageLogInfoFromImage(target)
	if info.ResponseModel != "gpt-image-2.6-sunburst" {
		t.Fatalf("imageUsageLogInfoFromImage 丢失 ResponseModel: %+v", info)
	}

	merged := mergeImageUsageLogInfo(imageUsageLogInfo{Count: 1}, info)
	if merged.ResponseModel != "gpt-image-2.6-sunburst" {
		t.Fatalf("mergeImageUsageLogInfo 丢失 ResponseModel: %+v", merged)
	}
}

// 回归：response.created 自报了 model 而 response.completed 未带时，
// 非流式路径不能把生命周期累积的观察丢在 firstMeta 里——imageLogInfo
// 必须携带该观察，成功用量日志才能记录 upstream_response_model 并回填
// effective_model。
func TestCollectImagesResponseKeepsCreatedModelObservationWithoutCompletedModel(t *testing.T) {
	upstream := `data: {"type":"response.created","response":{"model":"gpt-image-2.6-sunburst","tools":[{"type":"image_generation","model":"gpt-image-2"}],"created_at":1700000000}}` + "\n\n" +
		`data: {"type":"response.completed","response":{"created_at":1700000000,"tools":[{"type":"image_generation","model":"gpt-image-2","output_format":"png","size":"1024x1024"}],"output":[{"type":"image_generation_call","result":"` + tinyPNGBase64 + `","output_format":"png"}]}}` + "\n\n"

	_, _, imageCount, imageLogInfo, err := collectImagesResponse(context.Background(), strings.NewReader(upstream), "b64_json", "gpt-image-2", nil, imageUpscalePlan{})
	if err != nil {
		t.Fatalf("collectImagesResponse returned error: %v", err)
	}
	if imageCount != 1 {
		t.Fatalf("imageCount = %d, want 1", imageCount)
	}
	if imageLogInfo.ResponseModel != "gpt-image-2.6-sunburst" {
		t.Fatalf("imageLogInfo.ResponseModel = %q, want gpt-image-2.6-sunburst（created 自报不丢）", imageLogInfo.ResponseModel)
	}

	logInput := &database.UsageLogInput{Model: "gpt-image-2"}
	applyImageUpstreamModelObservation(logInput, imageLogInfo, "gpt-image-2", "gpt-image-2")
	if logInput.UpstreamResponseModel != "gpt-image-2.6-sunburst" {
		t.Fatalf("upstream_response_model = %q, want gpt-image-2.6-sunburst", logInput.UpstreamResponseModel)
	}
	if logInput.EffectiveModel != "gpt-image-2.6-sunburst" {
		t.Fatalf("effective_model = %q, want gpt-image-2.6-sunburst", logInput.EffectiveModel)
	}
}
