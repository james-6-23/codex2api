package config

import (
	"fmt"
	"log"
	"os"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"time"

	"github.com/joho/godotenv"
)

// schemaNameRegex 限定 PostgreSQL schema 名为 ASCII 标识符，避免 DSN/DDL 注入。
var schemaNameRegex = regexp.MustCompile(`^[A-Za-z_][A-Za-z0-9_]*$`)

// IsValidSchemaName 校验 PostgreSQL schema 名（首字母为字母或下划线，余下为字母/数字/下划线，长度 ≤63）。
func IsValidSchemaName(name string) bool {
	if name == "" || len(name) > 63 {
		return false
	}
	return schemaNameRegex.MatchString(name)
}

// DatabaseConfig 数据库核心配置。
type DatabaseConfig struct {
	Driver   string
	Path     string
	Host     string
	Port     int
	User     string
	Password string
	DBName   string
	Schema   string // PostgreSQL schema（search_path）；空值保持数据库默认行为
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
	dsn := fmt.Sprintf("host=%s port=%d user=%s password=%s dbname=%s sslmode=%s",
		d.Host, d.Port, d.User, d.Password, d.DBName, sslMode)
	if d.Schema != "" {
		// 通过 PostgreSQL startup options 在连接启动时设置 search_path，覆盖连接池中的所有连接。
		// schema 已在 Load() 阶段做白名单校验，此处可安全拼接。
		dsn += fmt.Sprintf(" options='-c search_path=%s,public'", d.Schema)
	}
	return dsn
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
	Driver               string
	Redis                RedisConfig
	ResponseContextStore ResponseContextStoreConfig
}

// ResponseContextStoreConfig 是 Memory 驱动下 previous_response_id 上下文的本地磁盘持久层。
// Path 为空表示关闭；默认放在 SQLite 主库同目录的独立文件中，不进入主库备份。
type ResponseContextStoreConfig struct {
	Path     string
	TTL      time.Duration
	MaxBytes int64
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
	Port                      int
	BindAddress               string // 监听地址，默认 0.0.0.0（兼容 Docker / 反代 / 公网）；如需仅本机访问可设为 127.0.0.1
	AdminSecret               string
	AllowAnonymousV1          bool // 显式允许 /v1/* 在未配置 API Key 时无鉴权放行（默认禁止）
	APIKeyAuthCacheEnabled    bool
	MaxRequestBodySize        int
	RequestMemoryBudgetBytes  int64 // Process-local retained HTTP/WS request body budget.
	SchedulerMaxWaiters       int   // Process-local waiting request budget.
	SchedulerMaxWaitersPerKey int
	Database                  DatabaseConfig
	Cache                     CacheConfig
	UseWebsocket              bool     // 是否启用 WebSocket 传输
	CodexUpstreamTransport    string   // http|auto|ws，默认 http；USE_WEBSOCKET 作为旧开关兼容
	TrustedProxies            []string // Gin 可信反向代理 CIDR/IP；默认信任回环与私有网段以兼容 Docker 反代，none/off/false/0 表示禁用
}

// applyTimezone 让 TZ 环境变量(含 .env 里的)真正作用于自然日限额等本地时间语义。
// Go 的 time.Local 在进程首次格式化本地时间时就已锁定(main 里 config.Load 之前的
// 启动日志即触发),godotenv 事后 os.Setenv 无法再影响它,必须显式覆盖 time.Local。
func applyTimezone() {
	tz := strings.TrimSpace(os.Getenv("TZ"))
	tz = strings.TrimPrefix(tz, ":") // POSIX 允许 ":Asia/Shanghai" 写法
	if tz == "" {
		return
	}
	loc, err := time.LoadLocation(tz)
	if err != nil {
		log.Printf("警告: TZ=%q 无法解析(%v),继续使用 %s", tz, err, time.Local)
		return
	}
	time.Local = loc
}

// Load 从 .env 文件加载核心环境配置，支持环境变量覆盖
func Load(envPath string) (*Config, error) {
	// 尝试加载 .env 文件（可选，如果文件不存在则忽略并使用当前环境变量）
	if envPath == "" {
		envPath = ".env"
	}
	_ = godotenv.Load(envPath)
	applyTimezone()

	cfg := &Config{
		Port:               8080,
		MaxRequestBodySize: 48 * 1024 * 1024,
	}

	// Web服务端口
	if port := os.Getenv("CODEX_PORT"); port != "" {
		fmt.Sscanf(port, "%d", &cfg.Port)
	} else if port := os.Getenv("PORT"); port != "" {
		fmt.Sscanf(port, "%d", &cfg.Port)
	}
	cfg.AdminSecret = strings.TrimSpace(os.Getenv("ADMIN_SECRET"))
	cfg.AllowAnonymousV1 = parseBoolEnv(os.Getenv("CODEX_ALLOW_ANONYMOUS"))
	cfg.APIKeyAuthCacheEnabled = true
	if value := strings.TrimSpace(os.Getenv("CODEX_API_KEY_AUTH_CACHE_ENABLED")); value != "" {
		enabled, err := strconv.ParseBool(value)
		if err != nil {
			return nil, fmt.Errorf("CODEX_API_KEY_AUTH_CACHE_ENABLED must be a boolean")
		}
		cfg.APIKeyAuthCacheEnabled = enabled
	}
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
	cfg.RequestMemoryBudgetBytes = max(int64(128<<20), int64(cfg.MaxRequestBodySize))
	if value := strings.TrimSpace(os.Getenv("CODEX_REQUEST_MEMORY_BUDGET_MB")); value != "" {
		mb, err := strconv.ParseInt(value, 10, 64)
		if err != nil || mb <= 0 || mb > (1<<63-1)/(1<<20) {
			return nil, fmt.Errorf("CODEX_REQUEST_MEMORY_BUDGET_MB must be a positive MiB value")
		}
		cfg.RequestMemoryBudgetBytes = mb << 20
		if cfg.RequestMemoryBudgetBytes < int64(cfg.MaxRequestBodySize) {
			return nil, fmt.Errorf("CODEX_REQUEST_MEMORY_BUDGET_MB must be at least CODEX_MAX_REQUEST_BODY_SIZE_MB")
		}
	}
	cfg.TrustedProxies = parseTrustedProxiesEnv(os.Getenv("CODEX_TRUSTED_PROXIES"))
	for _, setting := range []struct {
		name   string
		target *int
	}{
		{"CODEX_SCHEDULER_MAX_WAITERS", &cfg.SchedulerMaxWaiters},
		{"CODEX_SCHEDULER_MAX_WAITERS_PER_KEY", &cfg.SchedulerMaxWaitersPerKey},
	} {
		if value := strings.TrimSpace(os.Getenv(setting.name)); value != "" {
			n, err := strconv.Atoi(value)
			if err != nil || n <= 0 {
				return nil, fmt.Errorf("%s must be a positive integer", setting.name)
			}
			*setting.target = n
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
	if v := strings.TrimSpace(os.Getenv("DATABASE_SCHEMA")); v != "" {
		if !IsValidSchemaName(v) {
			return nil, fmt.Errorf("非法的 DATABASE_SCHEMA: %q（仅允许字母、数字、下划线，且不能以数字开头，长度不超过 63）", v)
		}
		cfg.Database.Schema = v
	}
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
	responseContextStore, err := loadResponseContextStoreConfig(cfg.Cache.Driver, cfg.Database)
	if err != nil {
		return nil, err
	}
	cfg.Cache.ResponseContextStore = responseContextStore

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

func parseTrustedProxiesEnv(value string) []string {
	value = strings.TrimSpace(value)
	if value == "" {
		return []string{"127.0.0.1", "::1", "10.0.0.0/8", "172.16.0.0/12", "192.168.0.0/16"}
	}
	switch strings.ToLower(value) {
	case "0", "false", "off", "none", "no":
		return nil
	}

	parts := strings.FieldsFunc(value, func(r rune) bool {
		return r == ',' || r == ';' || r == '\n' || r == '\r' || r == '\t' || r == ' '
	})
	proxies := make([]string, 0, len(parts))
	for _, part := range parts {
		if proxy := strings.TrimSpace(part); proxy != "" {
			proxies = append(proxies, proxy)
		}
	}
	return proxies
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

const (
	defaultResponseContextStoreTTLHours = 48
	defaultResponseContextStoreMaxMB    = 2048
)

// loadResponseContextStoreConfig 解析 RESPONSE_CONTEXT_STORE_*。
//   - RESPONSE_CONTEXT_STORE_PATH：显式路径；off/none/false/0 关闭。未设置时，
//     Memory 缓存 + SQLite 主库默认使用主库同目录下的 response-context.db。
//   - RESPONSE_CONTEXT_STORE_TTL_HOURS：最短保留时长（1~720，默认 48）。
//   - RESPONSE_CONTEXT_STORE_MAX_MB：去重后条目正文总量上限（64~65536，默认 2048）。
func loadResponseContextStoreConfig(cacheDriver string, db DatabaseConfig) (ResponseContextStoreConfig, error) {
	result := ResponseContextStoreConfig{
		TTL:      defaultResponseContextStoreTTLHours * time.Hour,
		MaxBytes: int64(defaultResponseContextStoreMaxMB) << 20,
	}
	if value := strings.TrimSpace(os.Getenv("RESPONSE_CONTEXT_STORE_TTL_HOURS")); value != "" {
		hours, err := strconv.Atoi(value)
		if err != nil || hours < 1 || hours > 720 {
			return result, fmt.Errorf("RESPONSE_CONTEXT_STORE_TTL_HOURS 必须是 1~720 的整数: %q", value)
		}
		result.TTL = time.Duration(hours) * time.Hour
	}
	if value := strings.TrimSpace(os.Getenv("RESPONSE_CONTEXT_STORE_MAX_MB")); value != "" {
		mb, err := strconv.ParseInt(value, 10, 64)
		if err != nil || mb < 64 || mb > 65536 {
			return result, fmt.Errorf("RESPONSE_CONTEXT_STORE_MAX_MB 必须是 64~65536 的整数: %q", value)
		}
		result.MaxBytes = mb << 20
	}
	path, explicit := os.LookupEnv("RESPONSE_CONTEXT_STORE_PATH")
	path = strings.TrimSpace(path)
	if explicit && path != "" {
		switch strings.ToLower(path) {
		case "off", "none", "false", "0":
			return result, nil
		}
		if cacheDriver != "memory" {
			return result, fmt.Errorf("RESPONSE_CONTEXT_STORE_PATH 仅支持 memory 缓存驱动；redis 模式使用共享上下文后端")
		}
		result.Path = path
		return result, nil
	}
	// Redis 已经是共享后端；只在 Memory 驱动下默认启用本地持久层。
	if cacheDriver != "memory" || db.Driver != "sqlite" {
		return result, nil
	}
	if dir := sqliteDatabaseDir(db.Path); dir != "" {
		result.Path = filepath.Join(dir, "response-context.db")
	}
	return result, nil
}

// sqliteDatabaseDir 返回 SQLite 主库文件所在目录；内存库或无法识别的 DSN 返回空。
func sqliteDatabaseDir(path string) string {
	path = strings.TrimSpace(path)
	if path == "" || path == ":memory:" || strings.Contains(strings.ToLower(path), "mode=memory") {
		return ""
	}
	if strings.HasPrefix(strings.ToLower(path), "file:") {
		path = path[len("file:"):]
		if index := strings.IndexByte(path, '?'); index >= 0 {
			path = path[:index]
		}
	}
	if path == "" || path == ":memory:" {
		return ""
	}
	return filepath.Dir(path)
}
