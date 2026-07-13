package proxy

import (
	"errors"
	"fmt"
	"testing"
)

// TestClassifyStreamOutcomeFlagsWSCloseForAuthVerify 验证：WS 上游读流失败
// （如 close 1008 policy violation）会置 verifyAccountAuth，用于异步验证账号鉴权；
// 普通 transport 失败、下游断开、正常终止都不置位。
func TestClassifyStreamOutcomeFlagsWSCloseForAuthVerify(t *testing.T) {
	wsErr := fmt.Errorf("websocket read error: %w", errors.New("websocket: close 1008 (policy violation)"))
	if out := classifyStreamOutcome(nil, wsErr, nil, false); !out.verifyAccountAuth {
		t.Fatalf("WS close 应置 verifyAccountAuth，得到 %+v", out)
	}
	messageTooBig := errors.New("websocket read error: websocket: close 1009 (message too big)")
	if out := classifyStreamOutcome(nil, messageTooBig, nil, false); out.verifyAccountAuth || out.penalize {
		t.Fatalf("WS close 1009 不应触发鉴权探针或账号处罚，得到 %+v", out)
	}

	if out := classifyStreamOutcome(nil, errors.New("connection reset by peer"), nil, false); out.verifyAccountAuth {
		t.Fatal("普通 transport 失败不应触发鉴权验证")
	}

	if out := classifyStreamOutcome(nil, nil, nil, true); out.verifyAccountAuth {
		t.Fatal("正常终止不应触发鉴权验证")
	}

	if out := classifyStreamOutcome(errors.New("context canceled"), nil, nil, false); out.verifyAccountAuth {
		t.Fatal("下游断开不应触发鉴权验证")
	}
}

func TestIsWebsocketUpstreamClose(t *testing.T) {
	if !isWebsocketUpstreamClose(fmt.Errorf("websocket read error: %w", errors.New("websocket: close 1008 (policy violation)"))) {
		t.Fatal("应识别 WS 上游读失败")
	}
	if isWebsocketUpstreamClose(nil) {
		t.Fatal("nil 不应识别为 WS 关闭")
	}
	if isWebsocketUpstreamClose(errors.New("some http transport error")) {
		t.Fatal("非 WS 错误不应识别")
	}
}
