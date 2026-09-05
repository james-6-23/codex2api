package database

import (
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
)

// sqliteSchemaTemplate 指向一份已经跑完全部迁移的 SQLite 数据库文件。注册后，
// New("sqlite", path) 在目标文件尚不存在时直接拷贝该模板并跳过 schema 迁移。
//
// 只给测试进程用：几百个测试各自在 TempDir 里建库，每次都完整重放 700 行 DDL；
// 纯 Go 的 SQLite 引擎在 -race 下慢 ~25 倍，一次迁移要 1.2s，而拷贝模板只要 1ms。
// 生产启动不会注册模板，路径与行为完全不变。
var sqliteSchemaTemplate atomic.Pointer[string]

// UseSQLiteSchemaTemplate 注册（或用空串清除）模板文件路径。
func UseSQLiteSchemaTemplate(path string) {
	path = strings.TrimSpace(path)
	if path == "" {
		sqliteSchemaTemplate.Store(nil)
		return
	}
	sqliteSchemaTemplate.Store(&path)
}

// PrepareSQLiteSchemaTemplate 在临时目录里完整初始化一次 SQLite 库并注册为模板，
// 供各测试包的 TestMain 调用；返回的 cleanup 会注销模板并删除临时目录。
func PrepareSQLiteSchemaTemplate() (cleanup func(), err error) {
	dir, err := os.MkdirTemp("", "codex2api-sqlite-template-")
	if err != nil {
		return nil, err
	}
	cleanup = func() {
		UseSQLiteSchemaTemplate("")
		_ = os.RemoveAll(dir)
	}
	// 建模板时必须走真实迁移路径，先确保没有残留注册。
	UseSQLiteSchemaTemplate("")
	path := filepath.Join(dir, "schema-template.db")
	db, err := New("sqlite", path)
	if err != nil {
		cleanup()
		return nil, fmt.Errorf("初始化 SQLite 模板失败: %w", err)
	}
	// 把 WAL 内容合并回主文件，模板只需要 .db 单个文件即可完整复制。
	if _, err := db.conn.ExecContext(context.Background(), "PRAGMA wal_checkpoint(TRUNCATE)"); err != nil {
		_ = db.Close()
		cleanup()
		return nil, fmt.Errorf("检查点 SQLite 模板失败: %w", err)
	}
	if err := db.Close(); err != nil {
		cleanup()
		return nil, fmt.Errorf("关闭 SQLite 模板失败: %w", err)
	}
	UseSQLiteSchemaTemplate(path)
	return cleanup, nil
}

// sqliteSchemaTemplateTarget 判断原始 DSN 是否是一个"尚不存在的普通文件路径"。
// 只有这种情况才能安全套用模板：已存在的文件（可能是旧 schema，需要真实迁移）、
// :memory: 和带 file:/查询参数的 DSN 一律走原路径。
func sqliteSchemaTemplateTarget(rawDSN string) (string, bool) {
	if sqliteSchemaTemplate.Load() == nil {
		return "", false
	}
	path := strings.TrimSpace(rawDSN)
	if path == "" || path == ":memory:" || strings.Contains(path, "?") ||
		strings.HasPrefix(strings.ToLower(path), "file:") {
		return "", false
	}
	if _, err := os.Stat(path); !errors.Is(err, os.ErrNotExist) {
		return "", false
	}
	return path, true
}

// applySQLiteSchemaTemplate 把模板复制到 target；任何失败都返回 false，让调用方
// 退回真实迁移路径，绝不让一次失败的复制变成半截数据库。
func applySQLiteSchemaTemplate(target string) bool {
	tmpl := sqliteSchemaTemplate.Load()
	if tmpl == nil {
		return false
	}
	src, err := os.Open(*tmpl)
	if err != nil {
		return false
	}
	defer src.Close()
	dst, err := os.OpenFile(target, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o600)
	if err != nil {
		return false
	}
	if _, err := io.Copy(dst, src); err != nil {
		_ = dst.Close()
		_ = os.Remove(target)
		return false
	}
	if err := dst.Close(); err != nil {
		_ = os.Remove(target)
		return false
	}
	return true
}
