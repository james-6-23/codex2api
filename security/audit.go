package security

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"
)

// AuditLogger 安全审计日志记录器
type AuditLogger struct {
	mu         sync.Mutex
	file       *os.File
	logDir     string
	logFile    string
	maxSize    int64
	maxBackups int
}

var (
	defaultAuditLogger *AuditLogger
	auditOnce          sync.Once
)

const defaultAuditLogDir = "logs/security"

// GetAuditLogger 获取默认审计日志记录器
func GetAuditLogger() *AuditLogger {
	auditOnce.Do(func() {
		if FileLogsDisabled() {
			defaultAuditLogger = &AuditLogger{}
			return
		}
		defaultAuditLogger = NewAuditLogger(securityLogDir(), "audit.log", 100*1024*1024, 10)
	})
	return defaultAuditLogger
}

func securityLogDir() string {
	if dir := strings.TrimSpace(os.Getenv("SECURITY_LOG_DIR")); dir != "" {
		return dir
	}
	if dir := strings.TrimSpace(os.Getenv("LOG_DIR")); dir != "" {
		return filepath.Join(dir, "security")
	}
	return defaultAuditLogDir
}

// NewAuditLogger 创建新的审计日志记录器
func NewAuditLogger(logDir, logFile string, maxSize int64, maxBackups int) *AuditLogger {
	if maxSize <= 0 {
		maxSize = 100 * 1024 * 1024 // 100MB
	}
	if maxBackups <= 0 {
		maxBackups = 10
	}

	al := &AuditLogger{
		logDir:     logDir,
		logFile:    logFile,
		maxSize:    maxSize,
		maxBackups: maxBackups,
	}

	// 初始化日志文件
	if err := al.init(); err != nil {
		fmt.Fprintf(os.Stderr, "初始化审计日志失败: %v\n", err)
	}

	return al
}

// init 初始化日志文件
func (al *AuditLogger) init() error {
	if err := os.MkdirAll(al.logDir, 0750); err != nil {
		return fmt.Errorf("创建日志目录失败: %w", err)
	}

	logPath := filepath.Join(al.logDir, al.logFile)
	f, err := os.OpenFile(logPath, os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0640)
	if err != nil {
		return fmt.Errorf("打开日志文件失败: %w", err)
	}

	al.file = f
	return nil
}

// Write 写入审计日志
func (al *AuditLogger) Write(level, action, details string) {
	al.mu.Lock()
	defer al.mu.Unlock()

	if al.file == nil {
		return
	}

	timestamp := time.Now().Format(time.RFC3339)
	line := fmt.Sprintf("[%s] [%s] %s: %s\n", timestamp, level, action, details)

	// 检查是否需要轮转
	if al.shouldRotate() {
		al.rotate()
	}

	al.file.WriteString(line)
}

// shouldRotate 检查是否需要日志轮转
func (al *AuditLogger) shouldRotate() bool {
	if al.file == nil {
		return false
	}

	info, err := al.file.Stat()
	if err != nil {
		return false
	}

	return info.Size() >= al.maxSize
}

// rotate 执行日志轮转
func (al *AuditLogger) rotate() {
	if al.file != nil {
		if err := al.file.Close(); err != nil {
			fmt.Fprintf(os.Stderr, "关闭日志文件失败: %v\n", err)
		}
	}

	// 轮转旧日志
	logPath := filepath.Join(al.logDir, al.logFile)
	for i := al.maxBackups - 1; i > 0; i-- {
		oldPath := fmt.Sprintf("%s.%d", logPath, i)
		newPath := fmt.Sprintf("%s.%d", logPath, i+1)
		if err := os.Rename(oldPath, newPath); err != nil && !os.IsNotExist(err) {
			fmt.Fprintf(os.Stderr, "重命名日志文件失败 %s -> %s: %v\n", oldPath, newPath, err)
		}
	}

	// 重命名当前日志
	if err := os.Rename(logPath, logPath+".1"); err != nil && !os.IsNotExist(err) {
		fmt.Fprintf(os.Stderr, "重命名当前日志文件失败: %v\n", err)
	}

	// 创建新日志文件
	if err := al.init(); err != nil {
		fmt.Fprintf(os.Stderr, "初始化新日志文件失败: %v\n", err)
	}
}

// Close 关闭审计日志
func (al *AuditLogger) Close() error {
	al.mu.Lock()
	defer al.mu.Unlock()

	if al.file != nil {
		return al.file.Close()
	}
	return nil
}

// SecurityAuditLog 记录安全审计日志（便捷函数）
func SecurityAuditLog(action, details string) {
	logger := GetAuditLogger()
	sanitizedDetails := SanitizeLog(details)
	logger.Write("SECURITY", action, sanitizedDetails)
}

// Info 记录信息级别审计日志
func Info(action, details string) {
	GetAuditLogger().Write("INFO", action, SanitizeLog(details))
}

// Warning 记录警告级别审计日志
func Warning(action, details string) {
	GetAuditLogger().Write("WARNING", action, SanitizeLog(details))
}

// Error 记录错误级别审计日志
func Error(action, details string) {
	GetAuditLogger().Write("ERROR", action, SanitizeLog(details))
}

// Critical 记录严重级别审计日志
func Critical(action, details string) {
	GetAuditLogger().Write("CRITICAL", action, SanitizeLog(details))
}
