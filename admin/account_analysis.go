package admin

import (
	"context"
	"math"
	"net/http"
	"sort"
	"strings"
	"time"

	"github.com/codex2api/auth"
	"github.com/codex2api/database"
	"github.com/gin-gonic/gin"
)

const (
	accountAnalysisCacheTTL             = 10 * time.Second
	analysisRPMPerConcurrencyDefault    = 6.0
	analysisRPMPerConcurrencyMin        = 1.0
	analysisRPMPerConcurrencyMax        = 30.0
	analysisRateLimitSaturationFraction = 0.3
	analysisRecentRateLimitWindow       = time.Hour
	analysisBulkLimitRatio              = 0.3
	analysisBulkLimitMinCount           = 3
	analysisPressureThresholdRPM        = 0.75
	analysisPressureThresholdActive     = 0.75
	analysisLowConfidenceThreshold      = 0.4
	analysisBurnMinElapsedRatio         = 0.05
	analysisSoonWindowRatio             = 0.2
)

type accountAnalysisCacheEntry struct {
	data      *accountAnalysisResponse
	expiresAt time.Time
}

type accountAnalysisTrafficCacheEntry struct {
	traffic   accountAnalysisTraffic
	expiresAt time.Time
}

type accountAnalysisResponse struct {
	Channel    string                             `json:"channel"`
	Quota      map[string]accountQuotaAnalysis    `json:"quota"`
	Recovery   map[string]accountRecoveryAnalysis `json:"recovery"`
	Reset      accountResetAnalysis               `json:"reset"`
	Forecasts  map[string]accountPressureForecast `json:"forecasts"`
	SnapshotAt string                             `json:"snapshot_at"`
	StatsState string                             `json:"stats_state"`
}

type accountQuotaAnalysis struct {
	Total       int                          `json:"total"`
	Sampled     int                          `json:"sampled"`
	Unsampled   int                          `json:"unsampled"`
	HighUsage   int                          `json:"high_usage"`
	Exhausted   int                          `json:"exhausted"`
	AverageUsed *float64                     `json:"average_used"`
	Buckets     []accountQuotaAnalysisBucket `json:"buckets"`
}

type accountQuotaAnalysisBucket struct {
	Min   int `json:"min"`
	Max   int `json:"max"`
	Count int `json:"count"`
}

type accountRecoveryAnalysis struct {
	Total       int                     `json:"total"`
	Recoverable int                     `json:"recoverable"`
	Unknown     int                     `json:"unknown"`
	NextAt      *int64                  `json:"next_at"`
	Buckets     []accountAnalysisBucket `json:"buckets"`
}

type accountResetAnalysis struct {
	Total   int                     `json:"total"`
	Known   int                     `json:"known"`
	Unknown int                     `json:"unknown"`
	NextAt  *int64                  `json:"next_at"`
	Buckets []accountAnalysisBucket `json:"buckets"`
}

type accountAnalysisBucket struct {
	StartAt       int64 `json:"start_at"`
	EndAt         int64 `json:"end_at"`
	Count         int   `json:"count"`
	CooldownCount int   `json:"cooldown_count,omitempty"`
}

type accountPressureForecast struct {
	Sampled             int      `json:"sampled"`
	Threshold           int      `json:"threshold"`
	PredictedAt         *int64   `json:"predicted_at"`
	PredictedCount      int      `json:"predicted_count"`
	Unknown             int      `json:"unknown"`
	RPM                 float64  `json:"rpm"`
	EffectiveRPMLimit   float64  `json:"effective_rpm_limit"`
	RPMPressure         *float64 `json:"rpm_pressure"`
	ActivePressure      float64  `json:"active_pressure"`
	RateLimitPressure   float64  `json:"rate_limit_pressure"`
	DispatchableAccount int      `json:"dispatchable_accounts"`
	AverageConcurrency  float64  `json:"avg_concurrency"`
	HighPressureAt      *int64   `json:"high_pressure_at"`
	SupplyShortageAt    *int64   `json:"supply_shortage_at"`
	RiskLevel           string   `json:"risk_level"`
	Confidence          float64  `json:"confidence"`
}

type accountAnalysisTraffic struct {
	rpm           float64
	rpmLimit      float64
	avgDurationMs float64
}

type accountSupplyEvent struct {
	at          float64
	concurrency float64
	delta       float64
	paired      bool
}

// GetAccountAnalysis returns a fixed-size, cached aggregate for quota and
// recovery charts. It is deliberately separate from the paged list so opening
// the account page never waits for pool-wide chart work.
func (h *Handler) GetAccountAnalysis(c *gin.Context) {
	channel := parseUsageChannel(c)
	if channel == "" {
		channel = database.UpstreamChannelCodex
	}
	ctx, cancel := context.WithTimeout(c.Request.Context(), 15*time.Second)
	defer cancel()
	result, err := h.getAccountAnalysis(ctx, channel)
	if err != nil {
		writeInternalError(c, err)
		return
	}
	c.JSON(http.StatusOK, result)
}

func (h *Handler) getAccountAnalysis(ctx context.Context, channel string) (*accountAnalysisResponse, error) {
	now := time.Now()
	h.accountAnalysisCacheMu.RLock()
	cached := h.accountAnalysisCache[channel]
	if cached != nil && now.Before(cached.expiresAt) {
		h.accountAnalysisCacheMu.RUnlock()
		return cached.data, nil
	}
	h.accountAnalysisCacheMu.RUnlock()

	if cached != nil {
		h.refreshAccountAnalysisAsync(channel)
		stale := *cached.data
		stale.StatsState = "stale"
		return &stale, nil
	}

	h.accountAnalysisBuildMu.Lock()
	defer h.accountAnalysisBuildMu.Unlock()
	h.accountAnalysisCacheMu.RLock()
	cached = h.accountAnalysisCache[channel]
	if cached != nil && time.Now().Before(cached.expiresAt) {
		h.accountAnalysisCacheMu.RUnlock()
		return cached.data, nil
	}
	h.accountAnalysisCacheMu.RUnlock()
	return h.rebuildAccountAnalysis(ctx, channel)
}

func (h *Handler) refreshAccountAnalysisAsync(channel string) {
	if !h.accountAnalysisBuildMu.TryLock() {
		return
	}
	go func() {
		defer h.accountAnalysisBuildMu.Unlock()
		ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
		defer cancel()
		_, _ = h.rebuildAccountAnalysis(ctx, channel)
	}()
}

func (h *Handler) rebuildAccountAnalysis(ctx context.Context, channel string) (*accountAnalysisResponse, error) {
	gen := h.accountCachesGen.Load()
	snapshot, err := h.getAccountListSnapshot(ctx, channel)
	if err != nil {
		return nil, err
	}
	traffic, trafficState := h.getAccountAnalysisTrafficNonBlocking(channel)
	now := time.Now()
	result := buildAccountAnalysis(snapshot, traffic, now)
	result.StatsState = combineAccountStatsState(snapshot.StatsState, trafficState)
	if h.accountCachesGen.Load() != gen {
		// 构建期间发生账号变更:返回结果但不入缓存,下一次读取重建。
		return result, nil
	}
	h.accountAnalysisCacheMu.Lock()
	if h.accountAnalysisCache == nil {
		h.accountAnalysisCache = make(map[string]*accountAnalysisCacheEntry)
	}
	h.accountAnalysisCache[channel] = &accountAnalysisCacheEntry{
		data:      result,
		expiresAt: now.Add(accountAnalysisCacheTTL),
	}
	h.accountAnalysisCacheMu.Unlock()
	return result, nil
}

func (h *Handler) largeAccountList(channel string) bool {
	h.accountListCacheMu.RLock()
	defer h.accountListCacheMu.RUnlock()
	cached := h.accountListCache[channel]
	return cached != nil && len(cached.Items) > requestCountFullPoolScanMax
}

func (h *Handler) getAccountAnalysisTrafficNonBlocking(channel string) (accountAnalysisTraffic, string) {
	now := time.Now()
	h.accountAnalysisTrafficMu.RLock()
	cached := h.accountAnalysisTraffic[channel]
	h.accountAnalysisTrafficMu.RUnlock()
	if cached != nil && now.Before(cached.expiresAt) {
		return cached.traffic, "ready"
	}
	if h.largeAccountList(channel) {
		traffic := accountAnalysisTraffic{}
		if h.rateLimiter != nil {
			traffic.rpmLimit = float64(h.rateLimiter.GetRPM())
		}
		return traffic, "ready"
	}

	h.refreshAccountAnalysisTrafficAsync(channel)
	if cached != nil {
		return cached.traffic, "stale"
	}
	traffic := accountAnalysisTraffic{}
	if h.rateLimiter != nil {
		traffic.rpmLimit = float64(h.rateLimiter.GetRPM())
	}
	return traffic, "warming"
}

func (h *Handler) refreshAccountAnalysisTrafficAsync(channel string) {
	if !h.accountAnalysisTrafficBuildMu.TryLock() {
		return
	}
	go func() {
		defer h.accountAnalysisTrafficBuildMu.Unlock()
		ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
		defer cancel()

		traffic := accountAnalysisTraffic{}
		stats, err := h.getUsageStatsSummaryCached(ctx, time.Time{}, time.Time{}, channel)
		if err != nil {
			return
		}
		if stats != nil {
			traffic.rpm = stats.RPM
			traffic.avgDurationMs = stats.AvgDurationMs
		}
		if h.rateLimiter != nil {
			traffic.rpmLimit = float64(h.rateLimiter.GetRPM())
		}
		h.accountAnalysisTrafficMu.Lock()
		if h.accountAnalysisTraffic == nil {
			h.accountAnalysisTraffic = make(map[string]*accountAnalysisTrafficCacheEntry)
		}
		h.accountAnalysisTraffic[channel] = &accountAnalysisTrafficCacheEntry{
			traffic:   traffic,
			expiresAt: time.Now().Add(accountAnalysisCacheTTL),
		}
		h.accountAnalysisTrafficMu.Unlock()
	}()
}

func combineAccountStatsState(states ...string) string {
	result := "ready"
	for _, state := range states {
		switch state {
		case "warming":
			return "warming"
		case "stale":
			result = "stale"
		}
	}
	return result
}

func buildAccountAnalysis(snapshot *accountListSnapshot, traffic accountAnalysisTraffic, now time.Time) *accountAnalysisResponse {
	items := snapshot.Items
	return &accountAnalysisResponse{
		Channel: snapshot.Channel,
		Quota: map[string]accountQuotaAnalysis{
			"5h": buildAccountQuotaAnalysis(items, "5h"),
			"7d": buildAccountQuotaAnalysis(items, "7d"),
		},
		Recovery: map[string]accountRecoveryAnalysis{
			"5h": buildAccountRecoveryAnalysis(items, "5h", now),
			"7d": buildAccountRecoveryAnalysis(items, "7d", now),
		},
		Reset: buildAccountResetAnalysis(items, now),
		Forecasts: map[string]accountPressureForecast{
			"5h": estimateAccountPressureForecast(items, "5h", now, traffic),
			"7d": estimateAccountPressureForecast(items, "7d", now, traffic),
		},
		SnapshotAt: snapshot.BuiltAt.Format(time.RFC3339),
		StatsState: snapshot.StatsState,
	}
}

func buildAccountQuotaAnalysis(items []*accountListSnapshotItem, window string) accountQuotaAnalysis {
	result := accountQuotaAnalysis{Buckets: make([]accountQuotaAnalysisBucket, 10)}
	for index := range result.Buckets {
		result.Buckets[index] = accountQuotaAnalysisBucket{Min: index * 10, Max: (index + 1) * 10}
	}
	totalUsed := 0.0
	for _, item := range items {
		if item.Status == "unauthorized" || item.Status == "error" || item.OpenAIResponses || (window == "5h" && !accountList5hQuotaEligible(item)) {
			continue
		}
		result.Total++
		usage, ok := accountAnalysisUsage(item, window)
		if !ok {
			continue
		}
		used := clampFloat(usage, 0, 100)
		index := int(math.Floor(used / 10))
		if index >= len(result.Buckets) {
			index = len(result.Buckets) - 1
		}
		result.Buckets[index].Count++
		result.Sampled++
		totalUsed += used
		if used >= 90 {
			result.HighUsage++
		}
		if used >= 100 {
			result.Exhausted++
		}
	}
	result.Unsampled = result.Total - result.Sampled
	if result.Sampled > 0 {
		average := totalUsed / float64(result.Sampled)
		result.AverageUsed = &average
	}
	return result
}

func buildAccountRecoveryAnalysis(items []*accountListSnapshotItem, window string, now time.Time) accountRecoveryAnalysis {
	bucketCount := 7
	bucketDuration := 24 * time.Hour
	if window == "5h" {
		bucketCount = 5
		bucketDuration = time.Hour
	}
	result := accountRecoveryAnalysis{Buckets: make([]accountAnalysisBucket, bucketCount)}
	for index := range result.Buckets {
		start := now.Add(time.Duration(index) * bucketDuration)
		result.Buckets[index] = accountAnalysisBucket{
			StartAt: start.UnixMilli(),
			EndAt:   start.Add(bucketDuration).UnixMilli(),
		}
	}
	var next time.Time
	for _, item := range items {
		recoveryAt, cooldown := accountRecoveryAt(item, window, now)
		if recoveryAt.IsZero() {
			if accountAnalysisWindowRateLimited(item, window) {
				result.Unknown++
			}
			continue
		}
		result.Recoverable++
		if next.IsZero() || recoveryAt.Before(next) {
			next = recoveryAt
		}
		for index := range result.Buckets {
			bucket := &result.Buckets[index]
			at := recoveryAt.UnixMilli()
			if at >= bucket.StartAt && at < bucket.EndAt {
				bucket.Count++
				if cooldown {
					bucket.CooldownCount++
				}
				break
			}
		}
	}
	result.Total = result.Recoverable + result.Unknown
	result.NextAt = unixMilliPointer(next)
	return result
}

func buildAccountResetAnalysis(items []*accountListSnapshotItem, now time.Time) accountResetAnalysis {
	location := time.FixedZone("UTC+8", 8*60*60)
	localNow := now.In(location)
	start := time.Date(localNow.Year(), localNow.Month(), localNow.Day(), 0, 0, 0, 0, location)
	result := accountResetAnalysis{Buckets: make([]accountAnalysisBucket, 7)}
	for index := range result.Buckets {
		bucketStart := start.AddDate(0, 0, index)
		result.Buckets[index] = accountAnalysisBucket{
			StartAt: bucketStart.UnixMilli(),
			EndAt:   bucketStart.AddDate(0, 0, 1).UnixMilli(),
		}
	}
	var next time.Time
	for _, item := range items {
		if !accountAnalysisHasBurnPrediction(item, "7d") {
			continue
		}
		result.Total++
		if item.Reset7dAt.IsZero() || !item.Reset7dAt.After(now) {
			result.Unknown++
			continue
		}
		result.Known++
		if next.IsZero() || item.Reset7dAt.Before(next) {
			next = item.Reset7dAt
		}
		at := item.Reset7dAt.UnixMilli()
		for index := range result.Buckets {
			bucket := &result.Buckets[index]
			if at >= bucket.StartAt && at < bucket.EndAt {
				bucket.Count++
				break
			}
		}
	}
	result.NextAt = unixMilliPointer(next)
	return result
}

func accountRecoveryAt(item *accountListSnapshotItem, window string, now time.Time) (time.Time, bool) {
	if window == "5h" {
		if accountList5hQuotaEligible(item) && item.UsagePercent5hOK && item.UsagePercent5h >= 100 && item.Reset5hAt.After(now) {
			return item.Reset5hAt, false
		}
		if item.CooldownUntil.After(now) && accountAnalysisShortRateLimited(item) {
			return item.CooldownUntil, true
		}
		return time.Time{}, false
	}
	if item.UsagePercent7dOK && item.UsagePercent7d >= 100 && item.Reset7dAt.After(now) {
		return item.Reset7dAt, false
	}
	return time.Time{}, false
}

func accountAnalysisUsage(item *accountListSnapshotItem, window string) (float64, bool) {
	if window == "5h" {
		return item.UsagePercent5h, item.UsagePercent5hOK
	}
	return item.UsagePercent7d, item.UsagePercent7dOK
}

func accountAnalysisShortRateLimited(item *accountListSnapshotItem) bool {
	status := strings.ToLower(item.Status)
	reason := strings.ToLower(item.CooldownReason)
	return status == "rate_limited" || status == auth.ResponsesRateLimitedCooldownReason ||
		status == "rate_limited_5h" || status == "cooldown" ||
		reason == "rate_limited" || reason == auth.ResponsesRateLimitedCooldownReason ||
		reason == "rate_limited_5h"
}

func accountAnalysisWindowRateLimited(item *accountListSnapshotItem, window string) bool {
	if window == "5h" {
		return (accountList5hQuotaEligible(item) && item.UsagePercent5hOK && item.UsagePercent5h >= 100) || accountAnalysisShortRateLimited(item)
	}
	status := strings.ToLower(item.Status)
	reason := strings.ToLower(item.CooldownReason)
	return (item.UsagePercent7dOK && item.UsagePercent7d >= 100) || status == "usage_exhausted" ||
		status == "rate_limited_7d" || reason == "rate_limited_7d"
}

func accountAnalysisHasBurnPrediction(item *accountListSnapshotItem, window string) bool {
	if strings.EqualFold(item.Status, "unauthorized") || item.OpenAIResponses {
		return false
	}
	if window == "5h" {
		return accountList5hQuotaEligible(item) && item.UsagePercent5hOK
	}
	return true
}

func accountAnalysisInSupplyPool(item *accountListSnapshotItem, window string) bool {
	return !strings.EqualFold(item.Status, "unauthorized") && item.Enabled && !accountAnalysisWindowRateLimited(item, window)
}

func estimateAccountPressureForecast(items []*accountListSnapshotItem, window string, now time.Time, traffic accountAnalysisTraffic) accountPressureForecast {
	nowMs := float64(now.UnixMilli())
	windowMs := accountAnalysisDefaultWindowMs(window)
	rpmPerSlot := analysisRPMPerConcurrencyDefault
	if traffic.avgDurationMs > 0 && !math.IsNaN(traffic.avgDurationMs) && !math.IsInf(traffic.avgDurationMs, 0) {
		rpmPerSlot = clampFloat(60_000/traffic.avgDurationMs, analysisRPMPerConcurrencyMin, analysisRPMPerConcurrencyMax)
	}

	dispatchable := make([]*accountListSnapshotItem, 0, len(items))
	totalConcurrency := 0.0
	activeRequests := 0.0
	for _, item := range items {
		if accountAnalysisInSupplyPool(item, window) {
			dispatchable = append(dispatchable, item)
			totalConcurrency += accountAnalysisConcurrency(item)
			activeRequests += float64(item.ActiveRequests)
		}
	}
	averageConcurrency := 0.0
	if len(dispatchable) > 0 {
		averageConcurrency = totalConcurrency / float64(len(dispatchable))
	}
	activePressure := 0.0
	if totalConcurrency > 0 {
		activePressure = clampFloat(activeRequests/totalConcurrency, 0, 3)
	}

	currentlyLimited := 0
	for _, item := range items {
		if item.Enabled && !strings.EqualFold(item.Status, "unauthorized") && accountAnalysisWindowRateLimited(item, window) {
			currentlyLimited++
		}
	}
	recentlyLimited := 0
	for _, item := range dispatchable {
		if !item.LastRateLimitedAt.IsZero() && now.Sub(item.LastRateLimitedAt) <= analysisRecentRateLimitWindow {
			recentlyLimited++
		}
	}
	limitedDenominator := len(dispatchable) + currentlyLimited
	recentLimitedFraction := 0.0
	if limitedDenominator > 0 {
		recentLimitedFraction = float64(recentlyLimited+currentlyLimited) / float64(limitedDenominator)
	}
	rateLimitPressure := clampFloat(recentLimitedFraction/analysisRateLimitSaturationFraction, 0, 1)
	rpm := finiteNonNegative(traffic.rpm)
	configuredLimit := finiteNonNegative(traffic.rpmLimit)
	concurrencyLimit := 0.0
	if totalConcurrency > 0 {
		concurrencyLimit = math.Max(1, math.Round(totalConcurrency*rpmPerSlot))
	}
	effectiveLimit := accountAnalysisEffectiveRPMLimit(configuredLimit, concurrencyLimit)
	var rpmPressure *float64
	if effectiveLimit > 0 {
		value := rpm / effectiveLimit
		rpmPressure = &value
	}
	pressureFactor := accountAnalysisPressureFactor(pointerFloatValue(rpmPressure), activePressure, rateLimitPressure)

	projectedLimitTimes := make([]float64, 0)
	events := make([]accountSupplyEvent, 0)
	sampled := 0
	unknown := 0
	for _, item := range items {
		if !accountAnalysisHasBurnPrediction(item, window) {
			continue
		}
		inSupply := accountAnalysisInSupplyPool(item, window)
		concurrency := accountAnalysisConcurrency(item)
		usage, usageOK := accountAnalysisUsage(item, window)
		accountWindowMs := accountAnalysisWindowMs(item, window)
		resetAt, knownReset := accountAnalysisResetAt(item, window, now)
		resetAtMs := nowMs + accountWindowMs
		if knownReset {
			resetAtMs = float64(resetAt.UnixMilli())
		}
		if !inSupply {
			if knownReset {
				events = append(events, accountSupplyEvent{at: resetAtMs, concurrency: concurrency, delta: 1})
			}
			if !usageOK {
				unknown++
			}
			continue
		}
		if !usageOK || math.IsNaN(usage) || math.IsInf(usage, 0) {
			unknown++
			continue
		}
		sampled++
		used := clampFloat(usage, 0, 100)
		if used >= 100 {
			projectedLimitTimes = append(projectedLimitTimes, nowMs)
			events = append(events,
				accountSupplyEvent{at: nowMs, concurrency: concurrency, delta: -1},
				accountSupplyEvent{at: resetAtMs, concurrency: concurrency, delta: 1, paired: true},
			)
			continue
		}
		windowStartAt := resetAtMs - accountWindowMs
		elapsedMs := math.Max(60_000, nowMs-windowStartAt)
		if elapsedMs < accountWindowMs*analysisBurnMinElapsedRatio {
			continue
		}
		burnRate := used / elapsedMs
		if burnRate <= 0 {
			unknown++
			continue
		}
		predictedAt := nowMs + (100-used)/burnRate
		if !math.IsNaN(predictedAt) && !math.IsInf(predictedAt, 0) && predictedAt <= resetAtMs {
			projectedLimitTimes = append(projectedLimitTimes, predictedAt)
			events = append(events,
				accountSupplyEvent{at: predictedAt, concurrency: concurrency, delta: -1},
				accountSupplyEvent{at: resetAtMs, concurrency: concurrency, delta: 1, paired: true},
			)
		}
	}
	sort.Float64s(projectedLimitTimes)
	sort.Slice(events, func(i, j int) bool { return events[i].at < events[j].at })
	highPressureAt, shortageAt := accountAnalysisSupplyPressure(events, rpm, configuredLimit, totalConcurrency, nowMs, rpmPerSlot)
	minAccountsForRPM := 0
	if rpm > 0 && averageConcurrency > 0 {
		minAccountsForRPM = int(math.Ceil(rpm / (averageConcurrency * rpmPerSlot)))
	}
	capacityThreshold := 0
	if minAccountsForRPM > 0 && len(dispatchable) > 0 {
		capacityThreshold = maxInt(analysisBulkLimitMinCount, len(dispatchable)-minAccountsForRPM)
	} else {
		capacityThreshold = maxInt(analysisBulkLimitMinCount, int(math.Ceil(float64(sampled)*analysisBulkLimitRatio)))
	}
	threshold := 0
	if sampled > 0 {
		threshold = minInt(sampled, capacityThreshold)
	}
	quotaAt := accountAnalysisBulkLimitTime(events, threshold)
	var predictedAt *float64
	if quotaAt != nil {
		value := nowMs + ((*quotaAt - nowMs) / pressureFactor)
		predictedAt = &value
	}
	confidence := 0.0
	if sampled+unknown > 0 {
		confidence = float64(sampled) / float64(sampled+unknown)
	}
	risk := accountAnalysisForecastRisk(predictedAt, highPressureAt, shortageAt, nowMs, windowMs, rpmPressure, activePressure, rateLimitPressure, confidence)
	predictedCount := len(projectedLimitTimes)
	if quotaAt != nil {
		predictedCount = 0
		for _, value := range projectedLimitTimes {
			if value <= *quotaAt {
				predictedCount++
			}
		}
	}
	return accountPressureForecast{
		Sampled: sampled, Threshold: threshold, PredictedAt: roundedFloatPointer(predictedAt),
		PredictedCount: predictedCount, Unknown: unknown, RPM: rpm, EffectiveRPMLimit: effectiveLimit,
		RPMPressure: rpmPressure, ActivePressure: activePressure, RateLimitPressure: rateLimitPressure,
		DispatchableAccount: len(dispatchable), AverageConcurrency: averageConcurrency,
		HighPressureAt: roundedFloatPointer(highPressureAt), SupplyShortageAt: roundedFloatPointer(shortageAt),
		RiskLevel: risk, Confidence: confidence,
	}
}

func accountAnalysisDefaultWindowMs(window string) float64 {
	if window == "5h" {
		return float64((5 * time.Hour).Milliseconds())
	}
	return float64((7 * 24 * time.Hour).Milliseconds())
}

func accountAnalysisWindowMs(item *accountListSnapshotItem, window string) float64 {
	if window == "5h" {
		return float64((5 * time.Hour).Milliseconds())
	}
	if item.Window7dSeconds > 0 {
		return float64(item.Window7dSeconds * 1000)
	}
	return float64((7 * 24 * time.Hour).Milliseconds())
}

func accountAnalysisResetAt(item *accountListSnapshotItem, window string, now time.Time) (time.Time, bool) {
	value := item.Reset7dAt
	if window == "5h" {
		value = item.Reset5hAt
	}
	return value, !value.IsZero() && value.After(now)
}

func accountAnalysisConcurrency(item *accountListSnapshotItem) float64 {
	return clampFloat(float64(item.DynamicConcurrency), 1, 50)
}

func accountAnalysisEffectiveRPMLimit(configured, concurrency float64) float64 {
	if concurrency <= 0 {
		return 0
	}
	if configured > 0 {
		return math.Min(configured, concurrency)
	}
	return concurrency
}

func accountAnalysisPressureFactor(rpmPressure, activePressure, rateLimitPressure float64) float64 {
	boosts := []float64{
		math.Max(0, rpmPressure-analysisPressureThresholdRPM),
		math.Max(0, activePressure-analysisPressureThresholdActive),
		rateLimitPressure,
	}
	sort.Sort(sort.Reverse(sort.Float64Slice(boosts)))
	return clampFloat(1+boosts[0]+boosts[1]*0.5, 1, 2.5)
}

func accountAnalysisSupplyPressure(events []accountSupplyEvent, rpm, configuredLimit, totalConcurrency, nowMs, rpmPerSlot float64) (*float64, *float64) {
	if rpm <= 0 {
		return nil, nil
	}
	remaining := totalConcurrency
	capacity := accountAnalysisEffectiveRPMLimit(configuredLimit, math.Round(remaining*rpmPerSlot))
	pressure := math.Inf(1)
	if capacity > 0 {
		pressure = rpm / capacity
	}
	var high, shortage *float64
	if pressure >= 0.9 {
		value := nowMs
		high = &value
	}
	if pressure >= 1 {
		value := nowMs
		shortage = &value
	}
	for _, event := range events {
		remaining = math.Max(0, remaining+event.delta*event.concurrency)
		capacity = accountAnalysisEffectiveRPMLimit(configuredLimit, math.Round(remaining*rpmPerSlot))
		pressure = math.Inf(1)
		if capacity > 0 {
			pressure = rpm / capacity
		}
		if high == nil && pressure >= 0.9 {
			value := event.at
			high = &value
		}
		if shortage == nil && pressure >= 1 {
			value := event.at
			shortage = &value
			break
		}
	}
	return high, shortage
}

func accountAnalysisBulkLimitTime(events []accountSupplyEvent, threshold int) *float64 {
	if threshold <= 0 {
		return nil
	}
	exhausted := 0
	for _, event := range events {
		if event.delta < 0 {
			exhausted++
			if exhausted >= threshold {
				value := event.at
				return &value
			}
		} else if event.paired && exhausted > 0 {
			exhausted--
		}
	}
	return nil
}

func accountAnalysisForecastRisk(predictedAt, highAt, shortageAt *float64, nowMs, windowMs float64, rpmPressure *float64, activePressure, rateLimitPressure, confidence float64) string {
	soon := windowMs * analysisSoonWindowRatio
	if (shortageAt != nil && *shortageAt-nowMs <= soon) ||
		(confidence >= analysisLowConfidenceThreshold && predictedAt != nil && *predictedAt-nowMs <= soon) ||
		pointerFloatValue(rpmPressure) >= 1 || activePressure >= 0.9 || rateLimitPressure >= 0.8 {
		return "high"
	}
	if highAt != nil || (confidence >= analysisLowConfidenceThreshold && predictedAt != nil) ||
		pointerFloatValue(rpmPressure) >= 0.7 || activePressure >= 0.7 || rateLimitPressure >= 0.4 {
		return "medium"
	}
	return "low"
}

func unixMilliPointer(value time.Time) *int64 {
	if value.IsZero() {
		return nil
	}
	result := value.UnixMilli()
	return &result
}

func roundedFloatPointer(value *float64) *int64 {
	if value == nil {
		return nil
	}
	result := int64(math.Round(*value))
	return &result
}

func pointerFloatValue(value *float64) float64 {
	if value == nil {
		return 0
	}
	return *value
}

func finiteNonNegative(value float64) float64 {
	if math.IsNaN(value) || math.IsInf(value, 0) || value < 0 {
		return 0
	}
	return value
}

func clampFloat(value, minValue, maxValue float64) float64 {
	return math.Min(maxValue, math.Max(minValue, value))
}

func minInt(left, right int) int {
	if left < right {
		return left
	}
	return right
}
