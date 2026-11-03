package proxy

import (
	"testing"
)

// I2 回归测试：Gemini 入站方法必须显式决定投影模式，不能靠响应体是否带
// finishReason 来推测流式性，否则无 finishReason 的单包 body 会退化为逐帧
// Append，跳过全文 Snapshot 与尾检。
func TestGeminiRequestProjectorUsesExplicitMode(t *testing.T) {
	streamProj := geminiRequestProjector(true)
	if streamProj.mode != GeminiProjectorModeStream || !streamProj.isStream {
		t.Fatalf("stream request must use explicit stream projector, got mode=%v isStream=%v", streamProj.mode, streamProj.isStream)
	}

	nonStreamProj := geminiRequestProjector(false)
	if nonStreamProj.mode != GeminiProjectorModeNonStream || nonStreamProj.isStream {
		t.Fatalf("non-stream request must use explicit non-stream projector, got mode=%v isStream=%v", nonStreamProj.mode, nonStreamProj.isStream)
	}

	// 无 finishReason 的整包 body 也必须产出 Snapshot（全文扫描 + 尾检），不得退化为 Append。
	body := `{"candidates":[{"index":0,"content":{"role":"model","parts":[{"text":"plain"}]}}]}`
	obs, err := nonStreamProj.Project("", []byte(body))
	if err != nil {
		t.Fatalf("project error: %v", err)
	}
	if len(obs) != 1 || obs[0].Action != ActionSnapshot || obs[0].TerminalStage != "response.completed" {
		t.Fatalf("expected exactly one response.completed snapshot for finishReason-less body, got %+v", obs)
	}

	// 同一 body 交给流式投影必须保持逐帧 Append 语义（不得误退化为快照）。
	streamObs, err := streamProj.Project("", []byte(body))
	if err != nil {
		t.Fatalf("stream project error: %v", err)
	}
	if len(streamObs) != 1 || streamObs[0].Action != ActionAppend {
		t.Fatalf("expected stream projector to append a finishReason-less chunk, got %+v", streamObs)
	}
}
