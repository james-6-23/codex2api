//go:build race

package promptfilter

// raceDetectorEnabled 标记本次测试构建启用了 -race。本包生产代码没有 goroutine，
// 纯 CPU 的超大样本扫描测试在竞争检测下慢 20 倍以上却跑不出任何竞争，
// 这类测试只在普通构建里跑（见 skipHeavyFixtureUnderRace）。
const raceDetectorEnabled = true
