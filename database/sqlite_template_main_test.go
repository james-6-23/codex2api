package database

import (
	"log"
	"os"
	"testing"
)

// TestMain 只初始化一次 SQLite schema 模板：之后每个测试建库直接复制模板、跳过迁移，
// 把 -race 下每个测试 ~1.2s 的建库开销压到毫秒级。
func TestMain(m *testing.M) {
	cleanup, err := PrepareSQLiteSchemaTemplate()
	if err != nil {
		log.Printf("SQLite schema template unavailable, tests fall back to full migrations: %v", err)
		os.Exit(m.Run())
	}
	code := m.Run()
	cleanup()
	os.Exit(code)
}
