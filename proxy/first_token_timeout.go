package proxy

import (
	"context"
	"fmt"
	"sync"
	"sync/atomic"
	"time"
)

type firstTokenTimeoutGuard struct {
	timeout time.Duration
	cancel  context.CancelFunc
	timer   *time.Timer
	fired   atomic.Bool
	once    sync.Once
}

func newFirstTokenTimeoutGuard(timeout time.Duration, cancel context.CancelFunc) *firstTokenTimeoutGuard {
	if timeout <= 0 || cancel == nil {
		return nil
	}
	guard := &firstTokenTimeoutGuard{
		timeout: timeout,
		cancel:  cancel,
	}
	guard.timer = time.AfterFunc(timeout, func() {
		guard.fired.Store(true)
		cancel()
	})
	return guard
}

func (g *firstTokenTimeoutGuard) Stop() {
	if g == nil {
		return
	}
	g.once.Do(func() {
		if g.timer != nil {
			g.timer.Stop()
		}
	})
}

func (g *firstTokenTimeoutGuard) MarkEvent(eventType string) {
	if g == nil || !isFirstTokenEvent(eventType) {
		return
	}
	g.Stop()
}

func (g *firstTokenTimeoutGuard) MarkPayload(data []byte) {
	if g == nil || !isFirstTokenPayload(data) {
		return
	}
	g.Stop()
}

func (g *firstTokenTimeoutGuard) MarkFirstToken() {
	if g == nil {
		return
	}
	g.Stop()
}

// MarkProgress 在首个非生命周期帧（response.created / response.in_progress 之外的
// 任意帧）到来时解除首字看门狗。任何这样的帧都证明上游已经开始流式产出真实响应，
// 因此一个先输出结构/推理帧、内容 token 延迟到来的长推理请求不应被首字超时中断
// （issue #207）。当上游在超时内什么都不发（或只发 created/in_progress）时看门狗
// 仍会触发，这与 v2.2.7 之前"首个响应迹象"的语义一致。
func (g *firstTokenTimeoutGuard) MarkProgress(eventType string) {
	if g == nil || isPreContentLifecycleEvent(eventType) {
		return
	}
	g.Stop()
}

func (g *firstTokenTimeoutGuard) TimedOut() bool {
	return g != nil && g.fired.Load()
}

func firstTokenTimeoutOutcome(timeout time.Duration) streamOutcome {
	return streamOutcome{
		logStatusCode:  logStatusUpstreamStreamBreak,
		failureKind:    "timeout",
		failureMessage: fmt.Sprintf("上游首字超时：%s 内未收到首个响应事件", timeout.Round(time.Millisecond)),
		penalize:       true,
	}
}

func firstTokenTimeoutError(timeout time.Duration) error {
	return ErrUpstreamTimeout(fmt.Errorf("first token timeout after %s", timeout.Round(time.Millisecond)))
}

// firstTokenTimeoutForRequest 返回本轮请求应使用的首字超时。上下文压缩轮
// （请求体含 compaction_trigger）豁免看门狗：压缩需要先读入整段历史上下文
// 才吐出首个 compaction 输出项，首帧天然可能超过用户配置的较短阈值（如 30s），
// 而这一轮的 encrypted_content 绑定原账号，超时换号重试大概率继续失败并耗尽
// attempt，导致会话彻底废掉（issue #381）。故对压缩轮返回 0（关闭看门狗），
// 让其思考时间不受限；异常挂死仍由客户端自身超时兜底。
func firstTokenTimeoutForRequest(base time.Duration, isCompactionTrigger bool) time.Duration {
	if isCompactionTrigger {
		return 0
	}
	return base
}
