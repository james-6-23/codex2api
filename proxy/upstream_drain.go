package proxy

import (
	"context"
	"time"
)

// upstreamDrainTimeout 控制客户端断开后，最多继续读上游多久以提取 usage。
// 取值参考：上游 response.completed 一般在断开后 1-3 秒内到达；
// 5 秒兜住绝大多数情况，又不会在上游卡住时无限占用连接。
const upstreamDrainTimeout = 5 * time.Second

// newDrainableUpstreamContext 创建一个与客户端 context 解耦的上游 context，
// 用途：客户端断开后仍能再读 drainTimeout 时间，以便从上游 SSE 拿到
// response.completed 事件里的 usage（流式请求计费的关键）。
//
// 行为：
//   - 客户端 ctx 取消后，等待 drainTimeout 再 cancel 上游
//   - 如果在 drainTimeout 之前调用方主动 cancel，立即停止
//   - parent 为 nil 时退化到 context.Background
func newDrainableUpstreamContext(clientCtx context.Context, drainTimeout time.Duration) (context.Context, context.CancelFunc) {
	baseCtx := context.Background()
	if clientCtx != nil {
		// Keep request-scoped values such as the User-Agent audit recorder while
		// detaching cancellation and deadlines for the bounded drain window.
		baseCtx = context.WithoutCancel(clientCtx)
	}
	upstreamCtx, cancelUpstream := context.WithCancel(baseCtx)
	if clientCtx == nil || drainTimeout <= 0 {
		return upstreamCtx, cancelUpstream
	}
	go func() {
		select {
		case <-clientCtx.Done():
			timer := time.NewTimer(drainTimeout)
			defer timer.Stop()
			select {
			case <-timer.C:
				cancelUpstream()
			case <-upstreamCtx.Done():
			}
		case <-upstreamCtx.Done():
		}
	}()
	return upstreamCtx, cancelUpstream
}
