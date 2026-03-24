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
	"syscall"
	"time"

	"github.com/codex2api/admin"
	"github.com/codex2api/auth"
	"github.com/codex2api/cache"
	"github.com/codex2api/config"
	"github.com/codex2api/database"
	"github.com/codex2api/proxy"
	"github.com/gin-gonic/gin"
)

//go:embed frontend/dist/*
var frontendFS embed.FS

func main() {
	log.SetFlags(log.LstdFlags | log.Lshortfile)
	log.Println("Codex2API v2 启动中...")

	// 1. 加载配置
	cfgPath := "config.yaml"
	if p := os.Getenv("CODEX_CONFIG"); p != "" {
		cfgPath = p
	}
	cfg, err := config.Load(cfgPath)
	if err != nil {
		log.Fatalf("加载配置失败: %v", err)
	}
	log.Printf("配置加载成功: port=%d", cfg.Port)

	// 2. 初始化 PostgreSQL
	db, err := database.New(cfg.Database.DSN())
	if err != nil {
		log.Fatalf("数据库初始化失败: %v", err)
	}
	defer db.Close()
	log.Printf("PostgreSQL 连接成功: %s:%d/%s", cfg.Database.Host, cfg.Database.Port, cfg.Database.DBName)

	// 3. 初始化 Redis
	tc, err := cache.New(cfg.Redis.Addr, cfg.Redis.Password, cfg.Redis.DB)
	if err != nil {
		log.Fatalf("Redis 初始化失败: %v", err)
	}
	defer tc.Close()
	log.Printf("Redis 连接成功: %s", cfg.Redis.Addr)

	// 4. 初始化账号管理器
	store := auth.NewStore(cfg, db, tc)

	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Minute)
	if err := store.Init(ctx, cfg); err != nil {
		cancel()
		log.Fatalf("账号初始化失败: %v", err)
	}
	cancel()

	// 启动后台刷新
	store.StartBackgroundRefresh()
	defer store.Stop()

	log.Printf("账号就绪: %d/%d 可用", store.AvailableCount(), store.AccountCount())

	// 5. 启动 HTTP 服务
	gin.SetMode(gin.ReleaseMode)
	r := gin.New()
	r.Use(gin.Recovery())
	r.Use(loggerMiddleware())

	handler := proxy.NewHandler(store, cfg.APIKeys, db)
	handler.RegisterRoutes(r)

	// 管理后台 API
	adminHandler := admin.NewHandler(store, db)
	adminHandler.RegisterRoutes(r)

	// 管理后台前端静态文件
	subFS, err := fs.Sub(frontendFS, "frontend/dist")
	if err != nil {
		log.Printf("前端静态文件加载失败（开发模式可忽略）: %v", err)
	} else {
		r.StaticFS("/admin", http.FS(subFS))
	}

	// 健康检查
	r.GET("/health", func(c *gin.Context) {
		c.JSON(200, gin.H{
			"status":    "ok",
			"available": store.AvailableCount(),
			"total":     store.AccountCount(),
		})
	})

	addr := fmt.Sprintf(":%d", cfg.Port)
	log.Println("==========================================")
	log.Printf("  Codex2API v2 已启动")
	log.Printf("  HTTP:   http://0.0.0.0%s", addr)
	log.Printf("  管理台: http://0.0.0.0%s/admin/", addr)
	log.Printf("  API:    POST /v1/chat/completions")
	log.Printf("  API:    POST /v1/responses")
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
	log.Println("已关闭")
}

// loggerMiddleware 简单日志中间件
func loggerMiddleware() gin.HandlerFunc {
	return func(c *gin.Context) {
		start := time.Now()
		c.Next()
		latency := time.Since(start)
		log.Printf("%s %s %d %v", c.Request.Method, c.Request.URL.Path, c.Writer.Status(), latency)
	}
}
