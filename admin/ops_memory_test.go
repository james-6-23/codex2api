package admin

import (
	"runtime"
	"strings"
	"testing"
)

func TestParseLinuxProcessRSS(t *testing.T) {
	status := "Name:\tcodex2api\nVmSize:\t1308916 kB\nVmRSS:\t53368 kB\nThreads:\t12\n"
	got, ok := parseLinuxProcessRSS(strings.NewReader(status))
	if !ok {
		t.Fatal("parseLinuxProcessRSS did not find VmRSS")
	}
	const want = uint64(53368 * 1024)
	if got != want {
		t.Fatalf("parseLinuxProcessRSS = %d, want %d", got, want)
	}
}

func TestParseLinuxProcessRSSRejectsMissingOrZeroValue(t *testing.T) {
	for _, status := range []string{
		"Name:\tcodex2api\nVmSize:\t1308916 kB\n",
		"Name:\tcodex2api\nVmRSS:\t0 kB\n",
	} {
		if got, ok := parseLinuxProcessRSS(strings.NewReader(status)); ok || got != 0 {
			t.Fatalf("parseLinuxProcessRSS(%q) = (%d, %t), want (0, false)", status, got, ok)
		}
	}
}

func TestParseCgroupLimit(t *testing.T) {
	cases := map[string]uint64{
		"max\n":                 0,
		"":                      0,
		"9223372036854771712\n": 0, // cgroup v1 未设限哨兵
		"536870912\n":           536870912,
		"not-a-number":          0,
	}
	for raw, want := range cases {
		if got := parseCgroupLimit(raw); got != want {
			t.Fatalf("parseCgroupLimit(%q) = %d, want %d", raw, got, want)
		}
	}
}

func TestParseCgroupStatValue(t *testing.T) {
	stat := "anon 1048576\nfile 2097152\ninactive_file 524288\nactive_file 262144\n"
	if got := parseCgroupStatValue(strings.NewReader(stat), "inactive_file"); got != 524288 {
		t.Fatalf("parseCgroupStatValue inactive_file = %d, want 524288", got)
	}
	if got := parseCgroupStatValue(strings.NewReader(stat), "total_inactive_file"); got != 0 {
		t.Fatalf("parseCgroupStatValue missing key = %d, want 0", got)
	}
}

func TestCollectOpsMemoryContainerFields(t *testing.T) {
	restore := readContainerMemoryFn
	defer func() { readContainerMemoryFn = restore }()

	readContainerMemoryFn = func() (uint64, uint64, bool) {
		return 128 * 1024 * 1024, 512 * 1024 * 1024, true
	}
	got := collectOpsMemory(func(mem *runtime.MemStats) { *mem = runtime.MemStats{Sys: 42} })
	if got.ContainerSource != "cgroup" ||
		got.ContainerUsedBytes != 128*1024*1024 ||
		got.ContainerLimitBytes != 512*1024*1024 {
		t.Fatalf("container mapping = %+v", got)
	}
	if got.ContainerPercent < 24.9 || got.ContainerPercent > 25.1 {
		t.Fatalf("container percent = %f, want 25", got.ContainerPercent)
	}

	// 无 cgroup 时回退进程 RSS,分母用宿主总内存。
	readContainerMemoryFn = func() (uint64, uint64, bool) { return 0, 0, false }
	fallback := collectOpsMemory(func(mem *runtime.MemStats) { *mem = runtime.MemStats{Sys: 42} })
	if fallback.ContainerSource != "process" || fallback.ContainerUsedBytes != fallback.ProcessBytes {
		t.Fatalf("container fallback = %+v", fallback)
	}
}

func TestReadProcessMemoryReturnsValue(t *testing.T) {
	got := readProcessMemory()
	if got == 0 {
		t.Fatal("readProcessMemory returned 0")
	}
	if runtime.GOOS == "linux" {
		if rss, ok := parseLinuxProcessRSS(strings.NewReader("VmRSS:\t1 kB\n")); !ok || rss != 1024 {
			t.Fatalf("Linux VmRSS parser sanity check = (%d, %t)", rss, ok)
		}
	}
}
