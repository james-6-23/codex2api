package cache

import (
	"context"
	"encoding/json"
	"time"
)

// PoolStats 统一的缓存连接池状态表示。
// 对于内存缓存，这些值用于向管理后台暴露一致的观测接口。
type PoolStats struct {
	TotalConns uint32
	IdleConns  uint32
	StaleConns uint32
}

type SessionAffinityBinding struct {
	AccountID int64  `json:"account_id"`
	ProxyURL  string `json:"proxy_url,omitempty"`
}

// TokenCache 统一的 token 缓存、刷新锁与短期运行态缓存接口。
type TokenCache interface {
	Driver() string
	Label() string
	Close() error
	Ping(ctx context.Context) error
	Stats() PoolStats
	PoolSize() int
	SetPoolSize(n int)
	GetAccessToken(ctx context.Context, accountID int64) (string, error)
	SetAccessToken(ctx context.Context, accountID int64, token string, ttl time.Duration) error
	DeleteAccessToken(ctx context.Context, accountID int64) error
	AcquireRefreshLock(ctx context.Context, accountID int64, ttl time.Duration) (bool, error)
	ReleaseRefreshLock(ctx context.Context, accountID int64) error
	AcquireLease(ctx context.Context, namespace, key, owner string, ttl time.Duration) (bool, error)
	ReleaseLease(ctx context.Context, namespace, key, owner string) error
	WaitForRefreshComplete(ctx context.Context, accountID int64, timeout time.Duration) (string, error)
	SetSessionAffinity(ctx context.Context, key string, binding SessionAffinityBinding, ttl time.Duration) error
	GetSessionAffinity(ctx context.Context, key string) (SessionAffinityBinding, bool, error)
	DeleteSessionAffinity(ctx context.Context, key string, accountID int64) error
	SetResponseContext(ctx context.Context, responseID string, items []json.RawMessage, ttl time.Duration) error
	GetResponseContext(ctx context.Context, responseID string) ([]json.RawMessage, error)
	SetRuntime(ctx context.Context, namespace string, key string, value json.RawMessage, ttl time.Duration) error
	GetRuntime(ctx context.Context, namespace string, key string) (json.RawMessage, bool, error)
	DeleteRuntime(ctx context.Context, namespace string, key string) error
}
