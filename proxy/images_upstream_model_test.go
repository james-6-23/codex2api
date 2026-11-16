package proxy

import (
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
