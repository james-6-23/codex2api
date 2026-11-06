package database

import (
	"context"
	"strings"
	"time"
)

// 近实时流量快照:近 5 分钟按秒分桶,QPS/TPS 取最近 10 秒的均值,峰值取单秒最大值。
const (
	trafficSnapshotLookback = 5 * time.Minute
	trafficSnapshotWindow   = 10 * time.Second
)

// trafficSecondBuckets 是按 Unix 秒聚合的请求数与 token 数,库里的明细和内存缓冲共用。
type trafficSecondBuckets struct {
	requests map[int64]float64
	tokens   map[int64]float64
}

func newTrafficSecondBuckets() *trafficSecondBuckets {
	return &trafficSecondBuckets{requests: make(map[int64]float64), tokens: make(map[int64]float64)}
}

func (b *trafficSecondBuckets) add(sec int64, requests, tokens float64) {
	b.requests[sec] += requests
	b.tokens[sec] += tokens
}

func (b *trafficSecondBuckets) snapshot(now time.Time) *TrafficSnapshot {
	windowStart := now.Add(-trafficSnapshotWindow).Unix()
	result := &TrafficSnapshot{}
	var windowRequests, windowTokens float64
	for sec, requests := range b.requests {
		tokens := b.tokens[sec]
		result.QPSPeak = max(result.QPSPeak, requests)
		result.TPSPeak = max(result.TPSPeak, tokens)
		if sec >= windowStart {
			windowRequests += requests
			windowTokens += tokens
		}
	}
	result.QPS = windowRequests / trafficSnapshotWindow.Seconds()
	result.TPS = windowTokens / trafficSnapshotWindow.Seconds()
	return result
}

// GetTrafficSnapshot 获取近实时流量快照。
//
// created_at 记的是请求完成时刻(issue #857),最近一个刷盘间隔内完成的请求还在内存缓冲里、
// 没进库;只读库的话近 10 秒 QPS 会跟着刷盘节奏在真实值的一半到全值之间跳,所以把本实例
// 缓冲里的日志也按完成时刻算进来。多实例部署时其他实例的缓冲看不到,那部分仍有一个刷盘间隔的滞后。
func (db *DB) GetTrafficSnapshot(ctx context.Context) (*TrafficSnapshot, error) {
	now := time.Now()
	since := now.Add(-trafficSnapshotLookback)
	buckets := newTrafficSecondBuckets()
	var err error
	if db.isSQLite() {
		err = db.loadTrafficSecondsSQLite(ctx, since, buckets)
	} else {
		err = db.loadTrafficSecondsPostgres(ctx, since, buckets)
	}
	if err != nil {
		return nil, err
	}
	// 必须先读库、再读缓冲:flush 先把批次移出缓冲再提交事务,这个顺序下同一条日志
	// 不会既出现在查询结果里又留在缓冲里被数两次(最多在提交的瞬间漏数)。
	db.addBufferedTrafficSeconds(since, buckets)
	return buckets.snapshot(now), nil
}

func (db *DB) loadTrafficSecondsPostgres(ctx context.Context, since time.Time, buckets *trafficSecondBuckets) error {
	rows, err := db.conn.QueryContext(ctx, `
		SELECT
			FLOOR(EXTRACT(EPOCH FROM created_at))::bigint AS sec,
			COUNT(*)::float8,
			COALESCE(SUM(total_tokens), 0)::float8
		FROM usage_logs
		WHERE created_at >= $1
		  AND TRIM(COALESCE(internal_reason, '')) = ''
		GROUP BY 1`, since)
	if err != nil {
		return err
	}
	defer rows.Close()
	for rows.Next() {
		var sec int64
		var requests, tokens float64
		if err := rows.Scan(&sec, &requests, &tokens); err != nil {
			return err
		}
		buckets.add(sec, requests, tokens)
	}
	return rows.Err()
}

// addBufferedTrafficSeconds 把还没落库的日志按完成时刻计入快照,口径与库里的查询一致:
// 只算会落库的行(usage_log_mode 过滤掉的不算),排除内部请求。
func (db *DB) addBufferedTrafficSeconds(since time.Time, buckets *trafficSecondBuckets) {
	if db == nil {
		return
	}
	db.logMu.Lock()
	defer db.logMu.Unlock()
	for i := range db.logBuf {
		entry := &db.logBuf[i]
		if !entry.StoreUsageLog || strings.TrimSpace(entry.InternalReason) != "" || entry.CreatedAt.Before(since) {
			continue
		}
		buckets.add(entry.CreatedAt.Unix(), 1, float64(entry.TotalTokens))
	}
}
