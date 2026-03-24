package cache

import (
	"context"
	"fmt"
	"time"

	"github.com/redis/go-redis/v9"
)

// TokenCache Redis Token 缓存（参考 sub2api OpenAITokenCache 接口）
type TokenCache struct {
	client *redis.Client
}

// New 创建 Redis Token 缓存
func New(addr, password string, db int) (*TokenCache, error) {
	client := redis.NewClient(&redis.Options{
		Addr:     addr,
		Password: password,
		DB:       db,
	})

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	if err := client.Ping(ctx).Err(); err != nil {
		return nil, fmt.Errorf("Redis 连接失败: %w", err)
	}

	return &TokenCache{client: client}, nil
}

// Close 关闭 Redis 连接
func (tc *TokenCache) Close() error {
	return tc.client.Close()
}

// ==================== Access Token 缓存 ====================

func tokenKey(accountID int64) string {
	return fmt.Sprintf("codex:token:%d", accountID)
}

// GetAccessToken 获取缓存的 AT
func (tc *TokenCache) GetAccessToken(ctx context.Context, accountID int64) (string, error) {
	val, err := tc.client.Get(ctx, tokenKey(accountID)).Result()
	if err == redis.Nil {
		return "", nil // cache miss
	}
	return val, err
}

// SetAccessToken 缓存 AT
func (tc *TokenCache) SetAccessToken(ctx context.Context, accountID int64, token string, ttl time.Duration) error {
	return tc.client.Set(ctx, tokenKey(accountID), token, ttl).Err()
}

// DeleteAccessToken 删除缓存的 AT
func (tc *TokenCache) DeleteAccessToken(ctx context.Context, accountID int64) error {
	return tc.client.Del(ctx, tokenKey(accountID)).Err()
}

// ==================== 分布式刷新锁 ====================

func refreshLockKey(accountID int64) string {
	return fmt.Sprintf("codex:refresh_lock:%d", accountID)
}

// AcquireRefreshLock 获取刷新锁（防止并发刷新同一账号）
func (tc *TokenCache) AcquireRefreshLock(ctx context.Context, accountID int64, ttl time.Duration) (bool, error) {
	ok, err := tc.client.SetNX(ctx, refreshLockKey(accountID), "1", ttl).Result()
	return ok, err
}

// ReleaseRefreshLock 释放刷新锁
func (tc *TokenCache) ReleaseRefreshLock(ctx context.Context, accountID int64) error {
	return tc.client.Del(ctx, refreshLockKey(accountID)).Err()
}

// ==================== 等待锁释放 ====================

// WaitForRefreshComplete 等待另一个进程完成刷新（轮询锁 + 读取缓存）
func (tc *TokenCache) WaitForRefreshComplete(ctx context.Context, accountID int64, timeout time.Duration) (string, error) {
	deadline := time.Now().Add(timeout)
	for time.Now().Before(deadline) {
		// 检查锁是否还在
		exists, err := tc.client.Exists(ctx, refreshLockKey(accountID)).Result()
		if err != nil {
			return "", err
		}

		if exists == 0 {
			// 锁已释放，尝试读取新的 AT
			token, err := tc.GetAccessToken(ctx, accountID)
			if err != nil {
				return "", err
			}
			if token != "" {
				return token, nil
			}
		}

		select {
		case <-ctx.Done():
			return "", ctx.Err()
		case <-time.After(200 * time.Millisecond):
		}
	}
	return "", fmt.Errorf("等待刷新超时")
}
