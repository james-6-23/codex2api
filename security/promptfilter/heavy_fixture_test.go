package promptfilter

import "testing"

// skipHeavyFixtureUnderRace 用于单线程、纯 CPU 的超大编码/压缩样本测试：它们在
// -race 下每个要 3–45 秒，却没有任何并发可供检测。普通构建（CI 的 test 任务）
// 仍完整执行，语义覆盖不变。
func skipHeavyFixtureUnderRace(t *testing.T) {
	t.Helper()
	if raceDetectorEnabled {
		t.Skip("CPU-bound single-threaded fixture; covered by the non-race test job")
	}
}
