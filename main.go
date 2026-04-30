package main

import (
	"context"
	"embed"
	"fmt"
	"io/fs"
	"log"
	"net/http"
	"os"
	"os/signal"
	"strings"
	"syscall"
	"time"

	"github.com/codex2api/admin"
	"github.com/codex2api/api"
	"github.com/codex2api/auth"
	"github.com/codex2api/cache"
	"github.com/codex2api/config"
	"github.com/codex2api/database"
	"github.com/codex2api/proxy"
	"github.com/codex2api/proxy/wsrelay"
	"github.com/codex2api/security"
	"github.com/gin-gonic/gin"
)

//go:embed frontend/dist/*
var frontendFS embed.FS

func main() {
	log.SetFlags(log.LstdFlags | log.Lshortfile)
	log.Println("Codex2API v2 启动中...")

	// 1. 加载配置 (.env)
	cfg, err := config.Load(".env")
	if err != nil {
		log.Fatalf("加载核心环境配置失败 (请检查 .env 文件): %v", err)
	}
	log.Printf("物理层配置加载成功: port=%d, database=%s, cache=%s", cfg.Port, cfg.Database.Label(), cfg.Cache.Label())

	// 2. 初始化数据库
	db, err := database.New(cfg.Database.Driver, cfg.Database.DSN())
	if err != nil {
		log.Fatalf("数据库初始化失败: %v", err)
	}
	defer db.Close()
	switch cfg.Database.Driver {
	case "sqlite":
		log.Printf("%s 连接成功: %s", cfg.Database.Label(), cfg.Database.Path)
	default:
		log.Printf("%s 连接成功: %s:%d/%s", cfg.Database.Label(), cfg.Database.Host, cfg.Database.Port, cfg.Database.DBName)
	}

	// 3. 读取运行时的系统逻辑设置（需在缓存初始化之前，以获取连接池大小）
	sysCtx, sysCancel := context.WithTimeout(context.Background(), 5*time.Second)
	settings, err := db.GetSystemSettings(sysCtx)
	sysCancel()

	if err == nil && settings == nil {
		// 初次运行，保存初始安全设置到数据库
		log.Printf("初次运行，初始化系统默认设置...")
		settings = &database.SystemSettings{
			MaxConcurrency:                   2,
			GlobalRPM:                        0,
			TestModel:                        "gpt-5.4",
			TestConcurrency:                  50,
			BackgroundRefreshIntervalMinutes: 2,
			UsageProbeMaxAgeMinutes:          10,
			RecoveryProbeIntervalMinutes:     30,
			ProxyURL:                         "",
			PgMaxConns:                       50,
			RedisPoolSize:                    30,
			AutoCleanUnauthorized:            false,
			AutoCleanRateLimited:             false,
			PromptFilterMode:                 "monitor",
			PromptFilterThreshold:            50,
			PromptFilterStrictThreshold:      90,
			PromptFilterLogMatches:           true,
			PromptFilterMaxTextLength:        81920,
			PromptFilterCustomPatterns:       "[]",
			PromptFilterDisabledPatterns:     "[]",
		}
		_ = db.UpdateSystemSettings(context.Background(), settings)
	} else if err != nil {
		log.Printf("警告: 读取系统设置失败: %v，将采用安全后备策略", err)
		settings = &database.SystemSettings{
			MaxConcurrency:                   2,
			GlobalRPM:                        0,
			TestModel:                        "gpt-5.4",
			TestConcurrency:                  50,
			BackgroundRefreshIntervalMinutes: 2,
			UsageProbeMaxAgeMinutes:          10,
			RecoveryProbeIntervalMinutes:     30,
			PgMaxConns:                       50,
			RedisPoolSize:                    30,
			PromptFilterMode:                 "monitor",
			PromptFilterThreshold:            50,
			PromptFilterStrictThreshold:      90,
			PromptFilterLogMatches:           true,
			PromptFilterMaxTextLength:        81920,
			PromptFilterCustomPatterns:       "[]",
			PromptFilterDisabledPatterns:     "[]",
		}
	} else {
		log.Printf("已加载持久化业务设置: ProxyURL=%s, MaxConcurrency=%d, GlobalRPM=%d, PgMaxConns=%d, RedisPoolSize=%d",
			settings.ProxyURL, settings.MaxConcurrency, settings.GlobalRPM, settings.PgMaxConns, settings.RedisPoolSize)
	}

	// 4. 初始化缓存（使用数据库中保存的连接池大小）
	redisPoolSize := 30
	if settings.RedisPoolSize > 0 {
		redisPoolSize = settings.RedisPoolSize
	}
	var tc cache.TokenCache
	switch cfg.Cache.Driver {
	case "memory":
		tc = cache.NewMemory(redisPoolSize)
	default:
		tc, err = cache.NewRedisWithOptions(cache.RedisOptions{
			Addr:               cfg.Cache.Redis.Addr,
			Username:           cfg.Cache.Redis.Username,
			Password:           cfg.Cache.Redis.Password,
			DB:                 cfg.Cache.Redis.DB,
			PoolSize:           redisPoolSize,
			TLS:                cfg.Cache.Redis.TLS,
			InsecureSkipVerify: cfg.Cache.Redis.InsecureSkipVerify,
		})
		if err != nil {
			log.Fatalf("缓存初始化失败: %v", err)
		}
	}
	defer tc.Close()
	switch cfg.Cache.Driver {
	case "memory":
		log.Printf("%s 缓存已启用: pool_size=%d", cfg.Cache.Label(), redisPoolSize)
	default:
		log.Printf("%s 连接成功: %s, pool_size=%d", cfg.Cache.Label(), cache.RedactRedisAddr(cfg.Cache.Redis.Addr), redisPoolSize)
	}

	// 4b. 应用数据库连接池设置
	if settings.PgMaxConns > 0 {
		db.SetMaxOpenConns(settings.PgMaxConns)
		log.Printf("%s 连接池: max_conns=%d", cfg.Database.Label(), settings.PgMaxConns)
	}

	// 4c. 初始化 Resin 粘性代理池
	if settings.ResinURL != "" && settings.ResinPlatformName != "" {
		proxy.SetResinConfig(&proxy.ResinConfig{
			BaseURL:      settings.ResinURL,
			PlatformName: settings.ResinPlatformName,
		})
		// 注入 Resin URL 装饰器到 auth 包（避免 auth → proxy 循环依赖）
		auth.ResinRequestDecorator = func(targetURL, accountID string) string {
			return proxy.BuildReverseProxyURL(targetURL)
		}
	}

	// 5. 初始化账号管理器
	store := auth.NewStore(db, tc, settings)

	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Minute)
	if err := store.Init(ctx); err != nil {
		cancel()
		log.Fatalf("账号初始化失败: %v", err)
	}
	cancel()

	// 全局 RPM 限流器
	rateLimiter := proxy.NewRateLimiter(settings.GlobalRPM)
	adminHandler := admin.NewHandler(store, db, tc, rateLimiter, cfg.AdminSecret)
	// 初始化 admin handler 的连接池设置跟踪
	adminHandler.SetPoolSizes(settings.PgMaxConns, settings.RedisPoolSize)
	store.SetUsageProbeFunc(adminHandler.ProbeUsageSnapshot)

	// 启动后台刷新
	store.StartBackgroundRefresh()
	store.TriggerUsageProbeAsync()
	store.TriggerRecoveryProbeAsync()
	store.TriggerAutoCleanupAsync()
	defer store.Stop()

	log.Printf("账号就绪: %d/%d 可用", store.AvailableCount(), store.AccountCount())

	// 6. 启动 HTTP 服务
	gin.SetMode(gin.ReleaseMode)
	r := gin.New()
	r.Use(api.RecoveryMiddleware())
	r.Use(api.RequestContextMiddleware())
	r.Use(api.VersionMiddleware())
	security.MaxRequestBodySize = cfg.MaxRequestBodySize
	r.Use(security.RequestSizeLimiter(int64(security.MaxRequestBodySize)))
	r.Use(api.BodyCacheMiddleware())
	r.Use(api.CORSMiddleware())
	r.Use(api.SecurityHeadersMiddleware())
	r.Use(loggerMiddleware())
	r.Use(security.SecurityHeadersMiddleware())

	// handler 不再接收 cfg.APIKeys
	// 从环境变量读取 Codex 画像与 Beta 配置。
	deviceCfg := proxy.DeviceProfileConfigFromEnv(os.Getenv)
	handler := proxy.NewHandler(store, db, cfg, deviceCfg)

	// 注册 WebSocket 执行函数（避免 proxy ↔ wsrelay 循环依赖）
	proxy.WebsocketExecuteFunc = wsrelay.ExecuteRequestWebsocket

	r.Use(rateLimiter.Middleware())
	if settings.GlobalRPM > 0 {
		log.Printf("全局限流已生效: %d RPM", settings.GlobalRPM)
	}
	log.Printf("单账号并发上限: %d", settings.MaxConcurrency)

	handler.RegisterRoutes(r)
	adminHandler.RegisterRoutes(r)

	// 管理后台前端静态文件
	subFS, err := fs.Sub(frontendFS, "frontend/dist")
	if err != nil {
		log.Printf("前端静态文件加载失败（开发模式可忽略）: %v", err)
	} else {
		httpFS := http.FS(subFS)
		// 预读 index.html（SPA 回退时直接返回，避免 FileServer 重定向）
		indexHTML, _ := fs.ReadFile(subFS, "index.html")

		serveAdmin := func(c *gin.Context) {
			fp := c.Param("filepath")
			// 尝试打开请求的文件（排除目录和根路径）
			if fp != "/" && len(fp) > 1 {
				trimmed := fp[1:] // 去掉开头的 /
				if f, err := subFS.Open(trimmed); err == nil {
					fi, statErr := f.Stat()
					f.Close()
					if statErr == nil && !fi.IsDir() {
						c.FileFromFS(fp, httpFS)
						return
					}
				}
			}
			// 文件不存在或者是目录 → 直接返回 index.html 字节（让 React Router 处理）
			c.Data(http.StatusOK, "text/html; charset=utf-8", indexHTML)
		}

		// 同时处理 /admin 和 /admin/*，避免依赖自动补斜杠重定向。
		r.GET("/admin", serveAdmin)
		r.GET("/admin/*filepath", serveAdmin)
		r.HEAD("/admin", serveAdmin)
		r.HEAD("/admin/*filepath", serveAdmin)
	}

	// 根路径重定向到管理后台（使用 302 避免浏览器永久缓存）
	r.GET("/", func(c *gin.Context) {
		c.Redirect(http.StatusFound, "/admin/")
	})

	// 健康检查
	r.GET("/health", func(c *gin.Context) {
		c.JSON(200, gin.H{
			"status":    "ok",
			"available": store.AvailableCount(),
			"total":     store.AccountCount(),
		})
	})

	// 6.5 启动安全状态自检 banner
	printSecurityBanner(db, cfg, settings)

	addr := fmt.Sprintf("%s:%d", cfg.BindAddress, cfg.Port)
	displayHost := cfg.BindAddress
	if displayHost == "0.0.0.0" || displayHost == "::" {
		displayHost = "localhost"
	}
	log.Println("==========================================")
	log.Printf("  Codex2API v2 已启动")
	log.Printf("  Listen: %s", addr)
	log.Printf("  HTTP:   http://%s:%d", displayHost, cfg.Port)
	log.Printf("  管理台: http://%s:%d/admin/", displayHost, cfg.Port)
	log.Printf("  API:    POST /v1/chat/completions")
	log.Printf("  API:    POST /v1/responses")
	log.Printf("  API:    POST /v1/images/generations")
	log.Printf("  API:    POST /v1/images/edits")
	log.Printf("  API:    POST /v1/messages")
	log.Printf("  API:    GET  /v1/models")
	log.Println("==========================================")

	// 优雅关闭
	go func() {
		if err := r.Run(addr); err != nil {
			log.Fatalf("HTTP 服务启动失败: %v", err)
		}
	}()

	quit := make(chan os.Signal, 1)
	signal.Notify(quit, syscall.SIGINT, syscall.SIGTERM)
	<-quit

	log.Println("正在关闭...")
	store.Stop()
	wsrelay.ShutdownExecutor()
	proxy.CloseErrorLogger()
	log.Println("已关闭")
}

// loggerMiddleware 简单日志中间件（增强版，支持敏感信息脱敏）
func loggerMiddleware() gin.HandlerFunc {
	return func(c *gin.Context) {
		start := time.Now()
		c.Next()
		latency := time.Since(start)
		if shouldSkipAccessLog(c.Request.Method, c.Request.URL.Path, c.Writer.Status()) {
			return
		}

		email, _ := c.Get("x-account-email")
		proxyURL, _ := c.Get("x-account-proxy")
		modelVal, _ := c.Get("x-model")
		effortVal, _ := c.Get("x-reasoning-effort")
		tierVal, _ := c.Get("x-service-tier")

		emailStr := ""
		if e, ok := email.(string); ok && e != "" {
			// 脱敏邮箱
			emailStr = security.MaskEmail(e)
		}
		proxyStr := "no proxy"
		if p, ok := proxyURL.(string); ok && p != "" {
			proxyStr = security.SanitizeLog(p)
		}

		// 构建扩展标签
		var tags []string
		if m, ok := modelVal.(string); ok && m != "" {
			tags = append(tags, security.SanitizeLog(m))
		}
		if e, ok := effortVal.(string); ok && e != "" {
			tags = append(tags, "effort="+security.SanitizeLog(e))
		}
		if t, ok := tierVal.(string); ok && t == "fast" {
			tags = append(tags, "fast")
		}
		tagStr := ""
		if len(tags) > 0 {
			tagStr = " " + strings.Join(tags, " ")
		}

		if emailStr != "" {
			log.Printf("%s %s %d %v%s [%s] [%s]", c.Request.Method, c.Request.URL.Path, c.Writer.Status(), latency, tagStr, emailStr, proxyStr)
		} else {
			log.Printf("%s %s %d %v%s", c.Request.Method, c.Request.URL.Path, c.Writer.Status(), latency, tagStr)
		}
	}
}

// printSecurityBanner 启动时打印安全状态自检 banner：
//   - 不再自动生成 ADMIN_SECRET。若两端都空，则提示用户首次访问页面进行初始化。
//   - 检查 API Key 数量、监听地址、匿名开关，命中风险时给出对应提示。
func printSecurityBanner(db *database.DB, cfg *config.Config, settings *database.SystemSettings) {
	if db == nil || cfg == nil || settings == nil {
		return
	}

	envSecret := strings.TrimSpace(cfg.AdminSecret)
	dbSecret := strings.TrimSpace(settings.AdminSecret)
	needsBootstrap := envSecret == "" && dbSecret == ""

	apiKeyCount := 0
	{
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		if rows, err := db.ListAPIKeys(ctx); err == nil {
			apiKeyCount = len(rows)
		}
		cancel()
	}

	bind := strings.TrimSpace(cfg.BindAddress)
	publicBind := bind == "" || bind == "0.0.0.0" || bind == "::"
	const sep = "=========================================================="
	log.Println(sep)
	log.Println("[SECURITY] Codex2API 安全状态自检")
	log.Println(sep)

	switch {
	case needsBootstrap:
		log.Println("⚠ 尚未配置 ADMIN_SECRET（环境变量与数据库均为空）。")
		log.Printf("  请使用浏览器访问管理台 http://%s:%d/admin/ 完成首次初始化，", bannerDisplayHost(bind), cfg.Port)
		log.Println("  设置一个强随机的管理密钥；该密钥也将作为登录密钥。")
		log.Println("  在初始化完成之前，所有 /api/admin/* 接口（除初始化端点外）均返回 503。")
	case envSecret != "":
		log.Println("✓ ADMIN_SECRET 来源：环境变量 (.env)")
	default:
		log.Println("✓ ADMIN_SECRET 来源：数据库（如需修改请进入「设置」页面）")
	}

	if apiKeyCount == 0 {
		if cfg.AllowAnonymousV1 {
			log.Println("⚠ /v1/* 当前处于【匿名访问】模式（CODEX_ALLOW_ANONYMOUS=true）。")
			log.Println("  任何能访问端口的人均可调用 /v1/* 消耗你的账号池配额，请仅在内网/测试环境使用！")
		} else {
			log.Println("⚠ 尚未创建任何对外 API Key。/v1/* 接口在创建第一把 Key 之前会返回 503。")
			log.Println("  请进入管理台「API 密钥」页面创建至少一把 Key 后再对外提供服务。")
		}
	} else {
		log.Printf("✓ 已配置 %d 个对外 API Key，/v1/* 强制鉴权已生效。", apiKeyCount)
	}

	if publicBind {
		log.Printf("ℹ 监听地址 = %s （所有网卡，兼容 Docker / 反代 / 公网）。", bind)
		log.Println("  生产环境请确认已部署反向代理 + HTTPS、配置防火墙白名单，并使用强 ADMIN_SECRET 与 API Key。")
		log.Println("  如希望服务只在本机回环可达，可设置 CODEX_BIND=127.0.0.1。")
	} else {
		log.Printf("✓ 监听地址 = %s （受限访问）。", bind)
	}

	log.Println(sep)
}

func bannerDisplayHost(bind string) string {
	if bind == "" || bind == "0.0.0.0" || bind == "::" {
		return "<your-host>"
	}
	return bind
}

func shouldSkipAccessLog(method string, path string, status int) bool {
	if status >= http.StatusBadRequest {
		return false
	}
	if method == http.MethodGet && path == "/api/admin/health" {
		return true
	}
	if method == http.MethodGet && (path == "/api/admin/images/jobs" || strings.HasPrefix(path, "/api/admin/images/jobs/")) {
		return true
	}
	return false
}
