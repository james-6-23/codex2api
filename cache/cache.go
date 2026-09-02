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

// ResponseContextReadStatus classifies a bounded shared-backend lookup without
// changing the TokenCache compatibility interface.
type ResponseContextReadStatus uint8

const (
	ResponseContextReadMiss ResponseContextReadStatus = iota
	ResponseContextReadFound
	ResponseContextReadTooLarge
	ResponseContextReadCorrupt
)

// ResponseContextReadResult is returned by optional bounded response-context
// readers. Transport failures remain ordinary errors.
type ResponseContextReadResult struct {
	Status ResponseContextReadStatus
	Items  []json.RawMessage
}

// BoundedResponseContextReader is an additive capability for shared backends.
// Implementations may reject oversized wire values before deserialization.
// TokenCache implementations that do not provide it remain supported through
// GetResponseContext followed by a logical-size check in the proxy.
type BoundedResponseContextReader interface {
	GetResponseContextBounded(ctx context.Context, responseID string, maxWireBytes int64) (ResponseContextReadResult, error)
}

// RuntimeOwnerStore is an optional atomic ownership capability used by
// cross-instance coordinators. Claim replaces the current owner and returns
// the previous value; refresh and delete are compare-and-act operations so a
// stale cleanup can never remove a newer owner.
type RuntimeOwnerStore interface {
	ClaimRuntimeOwner(ctx context.Context, namespace, key string, owner []byte, ttl time.Duration) ([]byte, error)
	CompareAndRefreshRuntimeOwner(ctx context.Context, namespace, key string, expected []byte, ttl time.Duration) (bool, error)
	CompareAndDeleteRuntimeOwner(ctx context.Context, namespace, key string, expected []byte) (bool, error)
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
	// IncrRuntimeCounters 原子累加一组浮点计数器（同一 key 下的多个字段），并刷新过期时间。
	// Redis 驱动落成一次 pipeline 的 HINCRBYFLOAT，用于跨实例共享短窗口增量；
	// 内存驱动在单锁内完成，语义一致但只对本进程可见。
	IncrRuntimeCounters(ctx context.Context, namespace string, key string, deltas map[string]float64, ttl time.Duration) error
	// GetRuntimeCounters 读取 IncrRuntimeCounters 写入的全部字段。key 不存在时返回 nil。
	GetRuntimeCounters(ctx context.Context, namespace string, key string) (map[string]float64, error)
	// SharedAcrossInstances 表示运行态缓存是否跨实例共享（Redis 为 true，进程内内存为 false）。
	// 调用方据此决定是否值得为跨实例一致性付额外的往返开销。
	SharedAcrossInstances() bool
}
