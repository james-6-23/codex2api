package cache

import (
	"context"
	"crypto/sha256"
	"crypto/tls"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"log"
	"net"
	"net/url"
	"os"
	"strconv"
	"strings"
	"time"

	"github.com/redis/go-redis/v9"
)

// RedisOptions describes the Redis connection settings used by the token cache.
type RedisOptions struct {
	Addr               string
	Username           string
	Password           string
	DB                 int
	PoolSize           int
	TLS                bool
	InsecureSkipVerify bool
}

// redisTokenCache Redis Token 缓存（参考 sub2api OpenAITokenCache 接口）
type redisTokenCache struct {
	client *redis.Client
}

var _ RuntimeOwnerStore = (*redisTokenCache)(nil)

type redisResponseContextRecord struct {
	Items []json.RawMessage `json:"items"`
}

// NewRedis 创建 Redis Token 缓存（poolSize <= 0 时使用默认值）。
func NewRedis(addr, password string, db int, poolSize ...int) (TokenCache, error) {
	size := 0
	if len(poolSize) > 0 && poolSize[0] > 0 {
		size = poolSize[0]
	}
	return NewRedisWithOptions(RedisOptions{
		Addr:     addr,
		Password: password,
		DB:       db,
		PoolSize: size,
	})
}

// NewRedisWithOptions 创建 Redis Token 缓存，支持 redis:// / rediss:// URL 和 TLS。
func NewRedisWithOptions(cfg RedisOptions) (TokenCache, error) {
	opts, err := buildRedisClientOptions(cfg)
	if err != nil {
		return nil, err
	}
	client := redis.NewClient(opts)

	ping := func(ctx context.Context) error {
		pingCtx, cancel := context.WithTimeout(ctx, 5*time.Second)
		defer cancel()
		return client.Ping(pingCtx).Err()
	}
	if err := waitForRedisLoading(context.Background(), ping, redisLoadingWait(), time.Sleep); err != nil {
		_ = client.Close()
		return nil, fmt.Errorf("Redis 连接失败: %w%s", err, redisConnectionHint(err, opts.TLSConfig != nil))
	}

	return &redisTokenCache{client: client}, nil
}

// defaultRedisLoadingWait 默认等待 Redis 加载数据集的时长。
// 大体量 AOF/RDB（数十 GB）重启后加载需要数分钟，期间 Redis 接受连接
// 但对命令返回 -LOADING；直接失败会让进程陷入崩溃重启循环。
const defaultRedisLoadingWait = 10 * time.Minute

func redisLoadingWait() time.Duration {
	raw := strings.TrimSpace(os.Getenv("CODEX_REDIS_LOADING_WAIT_SECONDS"))
	if raw == "" {
		return defaultRedisLoadingWait
	}
	seconds, err := strconv.Atoi(raw)
	if err != nil || seconds < 0 {
		return defaultRedisLoadingWait
	}
	return time.Duration(seconds) * time.Second
}

// isRedisLoadingError 判断错误是否为 Redis 数据集加载中的 -LOADING 回复。
func isRedisLoadingError(err error) bool {
	if err == nil {
		return false
	}
	return strings.HasPrefix(strings.TrimSpace(err.Error()), "LOADING")
}

// waitForRedisLoading 执行 ping；仅当收到 -LOADING 回复时在 wait 时长内轮询重试，
// 其余错误（地址错误、密码错误、拒绝连接等）保持原有的快速失败语义。
func waitForRedisLoading(ctx context.Context, ping func(context.Context) error, wait time.Duration, sleep func(time.Duration)) error {
	err := ping(ctx)
	if err == nil || !isRedisLoadingError(err) {
		return err
	}
	if wait <= 0 {
		return err
	}
	log.Printf("Redis 正在加载数据集到内存，等待就绪（最长 %s）...", wait)
	const retryInterval = 2 * time.Second
	var elapsed time.Duration
	for attempt := 1; ; attempt++ {
		if elapsed >= wait {
			return fmt.Errorf("等待 Redis 加载数据集超时（%s）: %w", wait, err)
		}
		sleep(retryInterval)
		elapsed += retryInterval
		err = ping(ctx)
		if err == nil || !isRedisLoadingError(err) {
			return err
		}
		// 每 30 秒左右提示一次，避免刷屏。
		if attempt%15 == 0 {
			log.Printf("Redis 仍在加载数据集，继续等待...")
		}
	}
}

func buildRedisClientOptions(cfg RedisOptions) (*redis.Options, error) {
	addr := strings.TrimSpace(cfg.Addr)
	if addr == "" {
		return nil, fmt.Errorf("Redis 地址不能为空")
	}

	var opts *redis.Options
	if isRedisURL(addr) {
		parsed, err := redis.ParseURL(addr)
		if err != nil {
			return nil, fmt.Errorf("Redis URL 格式错误: %w", err)
		}
		opts = parsed
		if cfg.DB != 0 {
			opts.DB = cfg.DB
		}
	} else {
		opts = &redis.Options{
			Addr: addr,
			DB:   cfg.DB,
		}
	}

	if strings.TrimSpace(cfg.Username) != "" {
		opts.Username = strings.TrimSpace(cfg.Username)
	}
	if cfg.Password != "" {
		opts.Password = cfg.Password
	}
	if cfg.PoolSize > 0 {
		opts.PoolSize = cfg.PoolSize
	}
	if cfg.TLS || strings.HasPrefix(strings.ToLower(addr), "rediss://") {
		ensureRedisTLSConfig(opts, cfg.InsecureSkipVerify)
	} else if cfg.InsecureSkipVerify && opts.TLSConfig != nil {
		opts.TLSConfig.InsecureSkipVerify = true
	}
	return opts, nil
}

func isRedisURL(addr string) bool {
	lower := strings.ToLower(addr)
	return strings.HasPrefix(lower, "redis://") || strings.HasPrefix(lower, "rediss://")
}

func ensureRedisTLSConfig(opts *redis.Options, insecureSkipVerify bool) {
	if opts.TLSConfig == nil {
		opts.TLSConfig = &tls.Config{
			MinVersion: tls.VersionTLS12,
			ServerName: redisServerName(opts.Addr),
		}
	}
	if opts.TLSConfig.MinVersion == 0 {
		opts.TLSConfig.MinVersion = tls.VersionTLS12
	}
	if opts.TLSConfig.ServerName == "" {
		opts.TLSConfig.ServerName = redisServerName(opts.Addr)
	}
	if insecureSkipVerify {
		opts.TLSConfig.InsecureSkipVerify = true
	}
}

func redisServerName(addr string) string {
	host, _, err := net.SplitHostPort(addr)
	if err == nil {
		return strings.Trim(host, "[]")
	}
	return strings.Trim(addr, "[]")
}

func redisConnectionHint(err error, tlsEnabled bool) string {
	if tlsEnabled || err == nil {
		return ""
	}
	if err == io.EOF || strings.Contains(strings.ToLower(err.Error()), "eof") {
		return "（如果使用 Aiven / Upstash 等云 Redis，请使用 rediss:// 地址或设置 REDIS_TLS=true 启用 TLS）"
	}
	return ""
}

// RedactRedisAddr masks credentials in redis:// / rediss:// URLs for logs.
func RedactRedisAddr(addr string) string {
	parsed, err := url.Parse(addr)
	if err != nil || parsed.User == nil || !isRedisURL(addr) {
		return addr
	}
	username := parsed.User.Username()
	if _, hasPassword := parsed.User.Password(); hasPassword {
		parsed.User = url.UserPassword(username, "redacted")
	} else {
		parsed.User = url.User(username)
	}
	return parsed.String()
}

// Close 关闭 Redis 连接
func (tc *redisTokenCache) Driver() string {
	return "redis"
}

func (tc *redisTokenCache) Label() string {
	return "Redis"
}

func (tc *redisTokenCache) Close() error {
	return tc.client.Close()
}

// Ping 检查 Redis 连通性
func (tc *redisTokenCache) Ping(ctx context.Context) error {
	return tc.client.Ping(ctx).Err()
}

// Stats 返回 Redis 连接池状态
func (tc *redisTokenCache) Stats() PoolStats {
	stats := tc.client.PoolStats()
	return PoolStats{
		TotalConns: stats.TotalConns,
		IdleConns:  stats.IdleConns,
		StaleConns: stats.StaleConns,
	}
}

// PoolSize 返回连接池大小配置
func (tc *redisTokenCache) PoolSize() int {
	return tc.client.Options().PoolSize
}

// SetPoolSize 设置连接池大小（go-redis 不支持运行时调整，需重启生效）
// 此方法仅保存配置值用于持久化，实际生效需重启容器
func (tc *redisTokenCache) SetPoolSize(n int) {
	// go-redis v9 的 PoolSize 在创建后不可变更
	// 此处仅做记录，重启后 main.go 会使用数据库中保存的值
	_ = n
}

// ==================== Access Token 缓存 ====================

func tokenKey(accountID int64) string {
	return fmt.Sprintf("codex:token:%d", accountID)
}

// GetAccessToken 获取缓存的 AT
func (tc *redisTokenCache) GetAccessToken(ctx context.Context, accountID int64) (string, error) {
	val, err := tc.client.Get(ctx, tokenKey(accountID)).Result()
	if err == redis.Nil {
		return "", nil // cache miss
	}
	return val, err
}

// SetAccessToken 缓存 AT
func (tc *redisTokenCache) SetAccessToken(ctx context.Context, accountID int64, token string, ttl time.Duration) error {
	return tc.client.Set(ctx, tokenKey(accountID), token, ttl).Err()
}

// DeleteAccessToken 删除缓存的 AT
func (tc *redisTokenCache) DeleteAccessToken(ctx context.Context, accountID int64) error {
	return tc.client.Del(ctx, tokenKey(accountID)).Err()
}

// ==================== 分布式刷新锁 ====================

func refreshLockKey(accountID int64) string {
	return fmt.Sprintf("codex:refresh_lock:%d", accountID)
}

// AcquireRefreshLock 获取刷新锁（防止并发刷新同一账号）
func (tc *redisTokenCache) AcquireRefreshLock(ctx context.Context, accountID int64, ttl time.Duration) (bool, error) {
	ok, err := tc.client.SetNX(ctx, refreshLockKey(accountID), "1", ttl).Result()
	return ok, err
}

// ReleaseRefreshLock 释放刷新锁
func (tc *redisTokenCache) ReleaseRefreshLock(ctx context.Context, accountID int64) error {
	return tc.client.Del(ctx, refreshLockKey(accountID)).Err()
}

func redisLeaseKey(namespace, key string) string {
	sum := sha256.Sum256([]byte(strings.TrimSpace(namespace) + "\x00" + strings.TrimSpace(key)))
	return "codex:lease:" + hex.EncodeToString(sum[:])
}

func (tc *redisTokenCache) AcquireLease(ctx context.Context, namespace, key, owner string, ttl time.Duration) (bool, error) {
	owner = strings.TrimSpace(owner)
	if owner == "" {
		return false, fmt.Errorf("lease owner is empty")
	}
	if ttl <= 0 {
		ttl = 30 * time.Second
	}
	return tc.client.SetNX(ctx, redisLeaseKey(namespace, key), owner, ttl).Result()
}

var releaseLeaseScript = redis.NewScript(`
if redis.call("GET", KEYS[1]) == ARGV[1] then
  return redis.call("DEL", KEYS[1])
end
return 0
`)

func (tc *redisTokenCache) ReleaseLease(ctx context.Context, namespace, key, owner string) error {
	return releaseLeaseScript.Run(ctx, tc.client, []string{redisLeaseKey(namespace, key)}, strings.TrimSpace(owner)).Err()
}

// ==================== 等待锁释放 ====================

// WaitForRefreshComplete 等待另一个进程完成刷新（轮询锁 + 读取缓存）
func (tc *redisTokenCache) WaitForRefreshComplete(ctx context.Context, accountID int64, timeout time.Duration) (string, error) {
	if timeout <= 0 {
		timeout = 30 * time.Second
	}

	timeoutTimer := time.NewTimer(timeout)
	defer timeoutTimer.Stop()

	ticker := time.NewTicker(200 * time.Millisecond)
	defer ticker.Stop()

	for {
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
		case <-timeoutTimer.C:
			return "", fmt.Errorf("等待刷新超时")
		case <-ticker.C:
		}
	}
}

// ==================== 运行态缓存 ====================

func runtimeHashKey(prefix, raw string) string {
	sum := sha256.Sum256([]byte(strings.TrimSpace(raw)))
	return prefix + hex.EncodeToString(sum[:])
}

func sessionAffinityKey(raw string) string {
	return runtimeHashKey("codex:session:", raw)
}

func responseContextKey(responseID string) string {
	return runtimeHashKey("codex:response:", responseID)
}

func runtimeNamespace(namespace string) string {
	namespace = strings.ToLower(strings.TrimSpace(namespace))
	if namespace == "" {
		return "default"
	}
	var b strings.Builder
	for _, r := range namespace {
		switch {
		case r >= 'a' && r <= 'z':
			b.WriteRune(r)
		case r >= '0' && r <= '9':
			b.WriteRune(r)
		case r == '-' || r == '_' || r == ':':
			b.WriteRune(r)
		default:
			b.WriteByte('_')
		}
	}
	if b.Len() == 0 {
		return "default"
	}
	return b.String()
}

func runtimeValueKey(namespace, raw string) string {
	return runtimeHashKey("codex:runtime:"+runtimeNamespace(namespace)+":", raw)
}

func (tc *redisTokenCache) SetSessionAffinity(ctx context.Context, key string, binding SessionAffinityBinding, ttl time.Duration) error {
	key = strings.TrimSpace(key)
	if key == "" || binding.AccountID == 0 {
		return nil
	}
	if ttl <= 0 {
		ttl = time.Hour
	}
	payload, err := json.Marshal(binding)
	if err != nil {
		return err
	}
	return tc.client.Set(ctx, sessionAffinityKey(key), payload, ttl).Err()
}

func (tc *redisTokenCache) GetSessionAffinity(ctx context.Context, key string) (SessionAffinityBinding, bool, error) {
	key = strings.TrimSpace(key)
	if key == "" {
		return SessionAffinityBinding{}, false, nil
	}
	val, err := tc.client.Get(ctx, sessionAffinityKey(key)).Result()
	if err == redis.Nil {
		return SessionAffinityBinding{}, false, nil
	}
	if err != nil {
		return SessionAffinityBinding{}, false, err
	}
	var binding SessionAffinityBinding
	if err := json.Unmarshal([]byte(val), &binding); err != nil {
		return SessionAffinityBinding{}, false, err
	}
	if binding.AccountID == 0 {
		return SessionAffinityBinding{}, false, nil
	}
	return binding, true, nil
}

func (tc *redisTokenCache) DeleteSessionAffinity(ctx context.Context, key string, accountID int64) error {
	key = strings.TrimSpace(key)
	if key == "" || accountID == 0 {
		return nil
	}
	const script = `
local value = redis.call("GET", KEYS[1])
if not value then
  return 0
end
local ok, data = pcall(cjson.decode, value)
if ok and tonumber(data["account_id"]) == tonumber(ARGV[1]) then
  return redis.call("DEL", KEYS[1])
end
return 0
`
	return tc.client.Eval(ctx, script, []string{sessionAffinityKey(key)}, accountID).Err()
}

func (tc *redisTokenCache) SetResponseContext(ctx context.Context, responseID string, items []json.RawMessage, ttl time.Duration) error {
	responseID = strings.TrimSpace(responseID)
	if responseID == "" {
		return nil
	}
	if ttl <= 0 {
		ttl = 10 * time.Minute
	}
	normalizedItems, err := NormalizeResponseContextItems(items)
	if err != nil {
		return err
	}
	record := redisResponseContextRecord{Items: normalizedItems}
	payload, err := json.Marshal(record)
	if err != nil {
		return err
	}
	return tc.client.Set(ctx, responseContextKey(responseID), payload, ttl).Err()
}

func (tc *redisTokenCache) GetResponseContext(ctx context.Context, responseID string) ([]json.RawMessage, error) {
	responseID = strings.TrimSpace(responseID)
	if responseID == "" {
		return nil, nil
	}
	val, err := tc.client.Get(ctx, responseContextKey(responseID)).Result()
	if err == redis.Nil {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	var record redisResponseContextRecord
	if err := json.Unmarshal([]byte(val), &record); err != nil {
		return nil, err
	}
	items := make([]json.RawMessage, len(record.Items))
	for i, item := range record.Items {
		items[i] = append(json.RawMessage(nil), item...)
	}
	normalizedItems, err := NormalizeResponseContextItems(items)
	if err != nil {
		return nil, err
	}
	return normalizedItems, nil
}

// GetResponseContextBounded reads at most maxWireBytes+1 bytes so an oversized
// Redis value can be rejected before the complete value is fetched or decoded.
func (tc *redisTokenCache) GetResponseContextBounded(ctx context.Context, responseID string, maxWireBytes int64) (ResponseContextReadResult, error) {
	responseID = strings.TrimSpace(responseID)
	if responseID == "" {
		return ResponseContextReadResult{Status: ResponseContextReadMiss}, nil
	}
	if maxWireBytes < 0 {
		maxWireBytes = 0
	}
	val, err := tc.client.GetRange(ctx, responseContextKey(responseID), 0, maxWireBytes).Bytes()
	if err == redis.Nil {
		return ResponseContextReadResult{Status: ResponseContextReadMiss}, nil
	}
	if err != nil {
		return ResponseContextReadResult{}, err
	}
	if len(val) == 0 {
		return ResponseContextReadResult{Status: ResponseContextReadMiss}, nil
	}
	if int64(len(val)) > maxWireBytes {
		return ResponseContextReadResult{Status: ResponseContextReadTooLarge}, nil
	}
	var record redisResponseContextRecord
	if err := json.Unmarshal(val, &record); err != nil {
		return ResponseContextReadResult{Status: ResponseContextReadCorrupt}, nil
	}
	if len(record.Items) == 0 {
		return ResponseContextReadResult{Status: ResponseContextReadMiss}, nil
	}
	normalizedItems, err := NormalizeResponseContextItems(record.Items)
	if err != nil {
		return ResponseContextReadResult{Status: ResponseContextReadCorrupt}, nil
	}
	return ResponseContextReadResult{Status: ResponseContextReadFound, Items: normalizedItems}, nil
}

func (tc *redisTokenCache) SetRuntime(ctx context.Context, namespace string, key string, value json.RawMessage, ttl time.Duration) error {
	key = strings.TrimSpace(key)
	if key == "" || len(value) == 0 {
		return nil
	}
	if ttl <= 0 {
		ttl = time.Minute
	}
	return tc.client.Set(ctx, runtimeValueKey(namespace, key), []byte(value), ttl).Err()
}

func (tc *redisTokenCache) GetRuntime(ctx context.Context, namespace string, key string) (json.RawMessage, bool, error) {
	key = strings.TrimSpace(key)
	if key == "" {
		return nil, false, nil
	}
	val, err := tc.client.Get(ctx, runtimeValueKey(namespace, key)).Bytes()
	if err == redis.Nil {
		return nil, false, nil
	}
	if err != nil {
		return nil, false, err
	}
	return append(json.RawMessage(nil), val...), true, nil
}

func (tc *redisTokenCache) DeleteRuntime(ctx context.Context, namespace string, key string) error {
	key = strings.TrimSpace(key)
	if key == "" {
		return nil
	}
	return tc.client.Del(ctx, runtimeValueKey(namespace, key)).Err()
}

var claimRuntimeOwnerScript = redis.NewScript(`
local previous = redis.call("GET", KEYS[1])
redis.call("SET", KEYS[1], ARGV[1], "PX", ARGV[2])
return previous
`)

var compareAndRefreshRuntimeOwnerScript = redis.NewScript(`
if redis.call("GET", KEYS[1]) == ARGV[1] then
  redis.call("PEXPIRE", KEYS[1], ARGV[2])
  return 1
end
return 0
`)

var compareAndDeleteRuntimeOwnerScript = redis.NewScript(`
if redis.call("GET", KEYS[1]) == ARGV[1] then
  return redis.call("DEL", KEYS[1])
end
return 0
`)

func runtimeOwnerTTLMillis(ttl time.Duration) int64 {
	if ttl <= 0 {
		ttl = time.Minute
	}
	millis := ttl.Milliseconds()
	if millis < 1 {
		return 1
	}
	return millis
}

func (tc *redisTokenCache) ClaimRuntimeOwner(ctx context.Context, namespace, key string, owner []byte, ttl time.Duration) ([]byte, error) {
	key = strings.TrimSpace(key)
	if key == "" || len(owner) == 0 {
		return nil, nil
	}
	result, err := claimRuntimeOwnerScript.Run(
		ctx,
		tc.client,
		[]string{runtimeValueKey(namespace, key)},
		owner,
		runtimeOwnerTTLMillis(ttl),
	).Result()
	if err != nil && err != redis.Nil {
		return nil, err
	}
	switch value := result.(type) {
	case nil:
		return nil, nil
	case string:
		return []byte(value), nil
	case []byte:
		return append([]byte(nil), value...), nil
	default:
		return nil, fmt.Errorf("unexpected runtime owner claim result %T", result)
	}
}

func (tc *redisTokenCache) CompareAndRefreshRuntimeOwner(ctx context.Context, namespace, key string, expected []byte, ttl time.Duration) (bool, error) {
	key = strings.TrimSpace(key)
	if key == "" || len(expected) == 0 {
		return false, nil
	}
	result, err := compareAndRefreshRuntimeOwnerScript.Run(
		ctx,
		tc.client,
		[]string{runtimeValueKey(namespace, key)},
		expected,
		runtimeOwnerTTLMillis(ttl),
	).Int64()
	return result == 1, err
}

func (tc *redisTokenCache) CompareAndDeleteRuntimeOwner(ctx context.Context, namespace, key string, expected []byte) (bool, error) {
	key = strings.TrimSpace(key)
	if key == "" || len(expected) == 0 {
		return false, nil
	}
	result, err := compareAndDeleteRuntimeOwnerScript.Run(
		ctx,
		tc.client,
		[]string{runtimeValueKey(namespace, key)},
		expected,
	).Int64()
	return result == 1, err
}

func (tc *redisTokenCache) IncrRuntimeCounters(ctx context.Context, namespace string, key string, deltas map[string]float64, ttl time.Duration) error {
	key = strings.TrimSpace(key)
	if key == "" || len(deltas) == 0 {
		return nil
	}
	if ttl <= 0 {
		ttl = time.Minute
	}
	hashKey := runtimeValueKey(namespace, key)
	_, err := tc.client.Pipelined(ctx, func(pipe redis.Pipeliner) error {
		for field, delta := range deltas {
			if delta == 0 {
				continue
			}
			pipe.HIncrByFloat(ctx, hashKey, field, delta)
		}
		pipe.Expire(ctx, hashKey, ttl)
		return nil
	})
	return err
}

func (tc *redisTokenCache) GetRuntimeCounters(ctx context.Context, namespace string, key string) (map[string]float64, error) {
	key = strings.TrimSpace(key)
	if key == "" {
		return nil, nil
	}
	raw, err := tc.client.HGetAll(ctx, runtimeValueKey(namespace, key)).Result()
	if err == redis.Nil {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	if len(raw) == 0 {
		return nil, nil
	}
	out := make(map[string]float64, len(raw))
	for field, value := range raw {
		parsed, convErr := strconv.ParseFloat(value, 64)
		if convErr != nil {
			continue
		}
		out[field] = parsed
	}
	return out, nil
}

// SharedAcrossInstances 报告 Redis 运行态缓存跨实例共享。
func (tc *redisTokenCache) SharedAcrossInstances() bool { return true }
