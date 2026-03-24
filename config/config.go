package config

import (
	"fmt"
	"os"
	"strconv"
	"strings"

	"gopkg.in/yaml.v3"
)

// AccountConfig 单个 Codex 账号配置（环境变量/YAML 直接导入用）
type AccountConfig struct {
	RefreshToken string `yaml:"refresh_token"`
	ProxyURL     string `yaml:"proxy_url"`
}

// DatabaseConfig PostgreSQL 配置
type DatabaseConfig struct {
	Host     string `yaml:"host"`
	Port     int    `yaml:"port"`
	User     string `yaml:"user"`
	Password string `yaml:"password"`
	DBName   string `yaml:"dbname"`
	SSLMode  string `yaml:"sslmode"`
}

// DSN 返回 PostgreSQL 连接字符串
func (d *DatabaseConfig) DSN() string {
	sslMode := d.SSLMode
	if sslMode == "" {
		sslMode = "disable"
	}
	return fmt.Sprintf("host=%s port=%d user=%s password=%s dbname=%s sslmode=%s",
		d.Host, d.Port, d.User, d.Password, d.DBName, sslMode)
}

// RedisConfig Redis 配置
type RedisConfig struct {
	Addr     string `yaml:"addr"`
	Password string `yaml:"password"`
	DB       int    `yaml:"db"`
}

// Config 全局配置
type Config struct {
	Port     int             `yaml:"port"`
	APIKeys  []string        `yaml:"api_keys"`
	ProxyURL string          `yaml:"proxy_url"`
	Database DatabaseConfig  `yaml:"database"`
	Redis    RedisConfig     `yaml:"redis"`
	Accounts []AccountConfig `yaml:"accounts"` // 可选：首次启动时通过 YAML/环境变量导入 RT 到数据库
}

// Load 从 YAML 文件加载配置，环境变量覆盖
func Load(path string) (*Config, error) {
	cfg := &Config{Port: 8080}

	// 尝试从文件加载
	if path != "" {
		data, err := os.ReadFile(path)
		if err != nil {
			if !os.IsNotExist(err) {
				return nil, fmt.Errorf("读取配置文件失败: %w", err)
			}
		} else {
			if err := yaml.Unmarshal(data, cfg); err != nil {
				return nil, fmt.Errorf("解析配置文件失败: %w", err)
			}
		}
	}

	// 环境变量覆盖
	if port := os.Getenv("CODEX_PORT"); port != "" {
		fmt.Sscanf(port, "%d", &cfg.Port)
	}
	if proxy := os.Getenv("CODEX_PROXY_URL"); proxy != "" {
		cfg.ProxyURL = proxy
	}
	if keys := os.Getenv("CODEX_API_KEYS"); keys != "" {
		cfg.APIKeys = strings.Split(keys, ",")
	}

	// 环境变量覆盖 Database 配置
	if dsn := os.Getenv("DATABASE_HOST"); dsn != "" {
		cfg.Database.Host = dsn
	}
	if v := os.Getenv("DATABASE_PORT"); v != "" {
		if p, err := strconv.Atoi(v); err == nil {
			cfg.Database.Port = p
		}
	}
	if v := os.Getenv("DATABASE_USER"); v != "" {
		cfg.Database.User = v
	}
	if v := os.Getenv("DATABASE_PASSWORD"); v != "" {
		cfg.Database.Password = v
	}
	if v := os.Getenv("DATABASE_NAME"); v != "" {
		cfg.Database.DBName = v
	}

	// 环境变量覆盖 Redis 配置
	if v := os.Getenv("REDIS_ADDR"); v != "" {
		cfg.Redis.Addr = v
	}
	if v := os.Getenv("REDIS_PASSWORD"); v != "" {
		cfg.Redis.Password = v
	}
	if v := os.Getenv("REDIS_DB"); v != "" {
		if db, err := strconv.Atoi(v); err == nil {
			cfg.Redis.DB = db
		}
	}

	// 从环境变量加载 RT 列表（逗号分隔）— 向后兼容 v1
	if rts := os.Getenv("CODEX_REFRESH_TOKENS"); rts != "" {
		for _, rt := range strings.Split(rts, ",") {
			rt = strings.TrimSpace(rt)
			if rt != "" {
				cfg.Accounts = append(cfg.Accounts, AccountConfig{RefreshToken: rt})
			}
		}
	}

	// 校验必需配置
	if cfg.Database.Host == "" {
		return nil, fmt.Errorf("必须配置 PostgreSQL (database.host 或 DATABASE_HOST)")
	}
	if cfg.Database.Port == 0 {
		cfg.Database.Port = 5432
	}
	if cfg.Database.SSLMode == "" {
		cfg.Database.SSLMode = "disable"
	}
	if cfg.Redis.Addr == "" {
		return nil, fmt.Errorf("必须配置 Redis (redis.addr 或 REDIS_ADDR)")
	}

	return cfg, nil
}
