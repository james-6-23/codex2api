package config

import (
	"fmt"
	"os"
	"strconv"
	"strings"

	"github.com/joho/godotenv"
)

// DatabaseConfig 数据库核心配置。
type DatabaseConfig struct {
	Driver   string
	Path     string
	Host     string
	Port     int
	User     string
	Password string
	DBName   string
	SSLMode  string
}

// DSN 返回当前驱动的连接字符串。
func (d *DatabaseConfig) DSN() string {
	if strings.EqualFold(d.Driver, "sqlite") {
		return d.Path
	}
	sslMode := d.SSLMode
	if sslMode == "" {
		sslMode = "disable"
	}
	return fmt.Sprintf("host=%s port=%d user=%s password=%s dbname=%s sslmode=%s",
		d.Host, d.Port, d.User, d.Password, d.DBName, sslMode)
}

// Label 返回用于展示的数据库标签。
func (d *DatabaseConfig) Label() string {
	if strings.EqualFold(d.Driver, "sqlite") {
		return "SQLite"
	}
	return "PostgreSQL"
}

// RedisConfig Redis 核心配置
type RedisConfig struct {
	Addr               string
	Username           string
	Password           string
	DB                 int
	TLS                bool
	InsecureSkipVerify bool
}

// CacheConfig 缓存核心配置。
type CacheConfig struct {
	Driver string
	Redis  RedisConfig
}

// Label 返回用于展示的缓存标签。
func (c *CacheConfig) Label() string {
	if strings.EqualFold(c.Driver, "memory") {
		return "Memory"
	}
	return "Redis"
}

// Config 全局核心环境配置（物理隔离的服务器参数）
// 业务逻辑参数（如 ProxyURL，APIKeys，MaxConcurrency）已全部移至数据库 SystemSettings 进行化
type Config struct {
	Port                   int
	BindAddress            string // 监听地址，默认 0.0.0.0（兼容 Docker / 反代 / 公网）；如需仅本机访问可设为 127.0.0.1
	AdminSecret            string
	AllowAnonymousV1       bool // 显式允许 /v1/* 在未配置 API Key 时无鉴权放行（默认禁止）
	MaxRequestBodySize     int
	Database               DatabaseConfig
	Cache                  CacheConfig
	UseWebsocket           bool   // 是否启用 WebSocket 传输
	CodexUpstreamTransport string // http|auto|ws，默认 http；USE_WEBSOCKET 作为旧开关兼容
}

// Load 从 .env 文件加载核心环境配置，支持环境变量覆盖
func Load(envPath string) (*Config, error) {
	// 尝试加载 .env 文件（可选，如果文件不存在则忽略并使用当前环境变量）
	if envPath == "" {
		envPath = ".env"
	}
	_ = godotenv.Load(envPath)

	cfg := &Config{
		Port:               8080,
		MaxRequestBodySize: 32 * 1024 * 1024,
	}

	// Web服务端口
	if port := os.Getenv("CODEX_PORT"); port != "" {
		fmt.Sscanf(port, "%d", &cfg.Port)
	} else if port := os.Getenv("PORT"); port != "" {
		fmt.Sscanf(port, "%d", &cfg.Port)
	}
	cfg.AdminSecret = strings.TrimSpace(os.Getenv("ADMIN_SECRET"))
	cfg.AllowAnonymousV1 = parseBoolEnv(os.Getenv("CODEX_ALLOW_ANONYMOUS"))
	// 默认绑 0.0.0.0 以兼容 Docker 端口映射、反向代理、生产服务器等常规部署。
	// 安全防护由 fail-closed 中间件 + 首启自助初始化 (/api/admin/bootstrap) + 启动 banner 共同保证；
	// 想要严格仅本机访问的用户可设 CODEX_BIND=127.0.0.1。
	cfg.BindAddress = strings.TrimSpace(os.Getenv("CODEX_BIND"))
	if cfg.BindAddress == "" {
		cfg.BindAddress = "0.0.0.0"
	}
	if v := strings.TrimSpace(os.Getenv("CODEX_MAX_REQUEST_BODY_SIZE_MB")); v != "" {
		if mb, err := strconv.Atoi(v); err == nil && mb > 0 {
			cfg.MaxRequestBodySize = mb * 1024 * 1024
		}
	}

	// Codex 上游传输配置。CODEX_UPSTREAM_TRANSPORT 优先；USE_WEBSOCKET 保留为旧开关。
	cfg.CodexUpstreamTransport = normalizeCodexUpstreamTransport(os.Getenv("CODEX_UPSTREAM_TRANSPORT"))
	if cfg.CodexUpstreamTransport == "" && parseBoolEnv(os.Getenv("USE_WEBSOCKET")) {
		cfg.CodexUpstreamTransport = "ws"
		cfg.UseWebsocket = true
	}
	if cfg.CodexUpstreamTransport == "" {
		cfg.CodexUpstreamTransport = "http"
	}
	if cfg.CodexUpstreamTransport == "ws" {
		cfg.UseWebsocket = true
	}

	// 数据库配置
	cfg.Database.Driver = normalizeDriver(os.Getenv("DATABASE_DRIVER"), "postgres")
	cfg.Database.Path = strings.TrimSpace(os.Getenv("DATABASE_PATH"))
	cfg.Database.Host = os.Getenv("DATABASE_HOST")
	if v := os.Getenv("DATABASE_PORT"); v != "" {
		if p, err := strconv.Atoi(v); err == nil {
			cfg.Database.Port = p
		}
	}
	cfg.Database.User = os.Getenv("DATABASE_USER")
	cfg.Database.Password = os.Getenv("DATABASE_PASSWORD")
	cfg.Database.DBName = os.Getenv("DATABASE_NAME")
	if v := os.Getenv("DATABASE_SSLMODE"); v != "" {
		cfg.Database.SSLMode = v
	}

	// 缓存配置
	cfg.Cache.Driver = normalizeDriver(os.Getenv("CACHE_DRIVER"), "redis")
	cfg.Cache.Redis.Addr = strings.TrimSpace(os.Getenv("REDIS_ADDR"))
	cfg.Cache.Redis.Username = strings.TrimSpace(os.Getenv("REDIS_USERNAME"))
	cfg.Cache.Redis.Password = os.Getenv("REDIS_PASSWORD")
	if v := os.Getenv("REDIS_DB"); v != "" {
		if db, err := strconv.Atoi(v); err == nil {
			cfg.Cache.Redis.DB = db
		}
	}
	cfg.Cache.Redis.TLS = parseBoolEnv(os.Getenv("REDIS_TLS"))
	cfg.Cache.Redis.InsecureSkipVerify = parseBoolEnv(os.Getenv("REDIS_INSECURE_SKIP_VERIFY"))

	// 校验必填物理层配置
	switch cfg.Database.Driver {
	case "sqlite":
		if cfg.Database.Path == "" {
			return nil, fmt.Errorf("必须通过 .env 或环境变量配置 SQLite 数据库路径 (DATABASE_PATH)")
		}
	case "postgres":
		if cfg.Database.Host == "" {
			return nil, fmt.Errorf("必须通过 .env 或环境变量配置 PostgreSQL (DATABASE_HOST)")
		}
	default:
		return nil, fmt.Errorf("不支持的数据库驱动: %s", cfg.Database.Driver)
	}
	if cfg.Database.Port == 0 {
		cfg.Database.Port = 5432
	}
	if cfg.Database.SSLMode == "" {
		cfg.Database.SSLMode = "disable"
	}

	switch cfg.Cache.Driver {
	case "memory":
	case "redis":
		if cfg.Cache.Redis.Addr == "" {
			return nil, fmt.Errorf("必须通过 .env 或环境变量配置 Redis (REDIS_ADDR)")
		}
	default:
		return nil, fmt.Errorf("不支持的缓存驱动: %s", cfg.Cache.Driver)
	}

	return cfg, nil
}

func normalizeDriver(value string, fallback string) string {
	if strings.TrimSpace(value) == "" {
		return fallback
	}
	return strings.ToLower(strings.TrimSpace(value))
}

func parseBoolEnv(value string) bool {
	switch strings.ToLower(strings.TrimSpace(value)) {
	case "1", "true", "yes", "y", "on":
		return true
	default:
		return false
	}
}

func normalizeCodexUpstreamTransport(value string) string {
	switch strings.ToLower(strings.TrimSpace(value)) {
	case "http", "https", "sse":
		return "http"
	case "auto":
		return "auto"
	case "ws", "websocket", "wss":
		return "ws"
	default:
		return ""
	}
}
