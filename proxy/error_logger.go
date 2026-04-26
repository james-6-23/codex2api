package proxy

import (
	"fmt"
	"log"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"

	"github.com/codex2api/security"
)

// fileLogger 单个日志文件实例
type fileLogger struct {
	once   sync.Once
	logger *log.Logger
	file   *os.File
	path   string
}

var (
	badRequestLogger  = &fileLogger{path: "bad_request.log"}  // 400 错误
	serverErrorLogger = &fileLogger{path: "server_error.log"} // 5xx 错误
)

const defaultLogDir = "logs"

func errorLogDir() string {
	if dir := strings.TrimSpace(os.Getenv("LOG_DIR")); dir != "" {
		return dir
	}
	return defaultLogDir
}

func (fl *fileLogger) init() *log.Logger {
	if security.FileLogsDisabled() {
		return nil
	}
	fl.once.Do(func() {
		dir := errorLogDir()
		if err := os.MkdirAll(dir, 0o755); err != nil {
			log.Printf("创建日志目录失败: %v", err)
			return
		}
		f, err := os.OpenFile(filepath.Join(dir, fl.path), os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0o644)
		if err != nil {
			log.Printf("打开日志文件 %s 失败: %v", fl.path, err)
			return
		}
		fl.file = f
		fl.logger = log.New(f, "", 0)
	})
	return fl.logger
}

func (fl *fileLogger) close() {
	if fl.file != nil {
		if err := fl.file.Close(); err != nil {
			fmt.Fprintf(os.Stderr, "关闭日志文件 %s 失败: %v\n", fl.path, err)
		}
	}
}

// writeEntry 写一条错误日志（自动脱敏敏感信息）
func (fl *fileLogger) writeEntry(endpoint string, statusCode int, model string, accountID int64, body []byte) {
	l := fl.init()
	if l == nil {
		return
	}

	// 脱敏日志内容
	safeEndpoint := security.SanitizeLog(endpoint)
	safeModel := security.SanitizeLog(model)
	bodyStr := string(body)

	// 检查并脱敏响应体中的敏感信息
	bodyStr = security.MaskSensitiveData(bodyStr)
	bodyStr = security.SafeTruncate(bodyStr, 5000) // 限制日志大小

	ts := time.Now().Format("2006/01/02 15:04:05")
	l.Printf("========== %s ==========\nEndpoint: %s\nStatus: %d\nModel: %s\nAccount: %d\nResponse:\n%s\n",
		ts, safeEndpoint, statusCode, safeModel, accountID, bodyStr)
}

// logUpstreamError 根据状态码分发到对应日志文件
func logUpstreamError(endpoint string, statusCode int, model string, accountID int64, body []byte) {
	switch {
	case statusCode == 400:
		badRequestLogger.writeEntry(endpoint, statusCode, model, accountID, body)
	case statusCode >= 500:
		serverErrorLogger.writeEntry(endpoint, statusCode, model, accountID, body)
	}
}

// CloseErrorLogger 关闭所有错误日志文件（程序退出时调用）
func CloseErrorLogger() {
	badRequestLogger.close()
	serverErrorLogger.close()
}
