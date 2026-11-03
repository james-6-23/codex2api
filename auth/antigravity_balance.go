package auth

import (
	"context"
	"hash/fnv"
	"math"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/codex2api/database"
)

// neutralQuotaPressure 是「关于额度一无所知」时的中性压力分：既不让无事实的账号
// 白嫖最高分（0），也不让它凭空降权（1）。
const neutralQuotaPressure = 0.5

// quotaPressureWindows 是参与压力聚合的白名单窗口，顺序即加权求和的遍历顺序。
// 白名单之外（daily/monthly/未知）不参与调度打分，与 NormalizeAntigravityWindow
// 的白名单保持一致。
var quotaPressureWindows = []string{"weekly", "5h"}

// aggregateQuotaWindow 聚合一个白名单窗口的桶状态：
//   - present=true：存在 presence 有效、数值合法且未过期的桶，value 取最小剩余；
//   - exists=true：权威完整清单（summary_integrity）证实窗口存在，但数值/时效不可用；
//   - 两者皆 false：缺席或存在性未知（调用方省略，不惩罚）。
//
// scope 限定参与聚合的桶（按请求模型的配额族），nil 表示整号所有桶。
func aggregateQuotaWindow(snap *AntigravityQuotaSnapshot, window string, scope antigravityQuotaBucketScope, now time.Time) (value float64, present, exists bool) {
	best := math.Inf(1)
	foundAny := false
	for _, g := range snap.Groups {
		for _, b := range g.Buckets {
			if scope != nil && !scope(b) {
				continue
			}
			w, ok := NormalizeAntigravityWindow(b.Window)
			if !ok || w != window {
				continue
			}
			foundAny = true
			if !b.FractionPresent || b.RemainingFraction == nil {
				continue
			}
			v := *b.RemainingFraction
			if v < 0.0 || v > 1.0 || math.IsNaN(v) || math.IsInf(v, 0) {
				continue
			}
			if !b.ObservedAt.IsZero() && now.After(b.ObservedAt.Add(time.Duration(b.TTLSeconds)*time.Second)) {
				continue // 时效不可用
			}
			if v < best {
				best = v
			}
			present = true
		}
	}
	if present {
		return best, true, false
	}
	if foundAny && snap.SourceMetadata["groups"].SummaryIntegrity {
		return 0, false, true
	}
	return 0, false, false
}

// quotaPressureQuantum 是压力分的量化网格（1e-12）。
//
// 「1 - 剩余比例」与「多窗口平均」在二进制浮点下会留下末位漂移：两个剩余 0.8 的窗口
// 求平均，裸结果是 0.19999999999999996 而不是 0.2。粒度 1e-12 比 usageWeight（0.5 量级）
// 低十个数量级，不改变任何排序结论，但让同一额度事实恒得同一分数——选号同分依赖哈希
// 打散，分数抖动会让同一批账号在两次选号间得到不同序。
const quotaPressureQuantum = 1e12

// quantizeQuotaPressure 把压力分对齐到 quotaPressureQuantum 网格。
func quantizeQuotaPressure(v float64) float64 {
	if math.IsNaN(v) || math.IsInf(v, 0) {
		return neutralQuotaPressure
	}
	return math.Round(v*quotaPressureQuantum) / quotaPressureQuantum
}

// ComputeQuotaPressure 把 raw 配额快照折算成 [0,1] 的调度压力分，用于替换选号打分里
// 的原 usage 项（Spec §7.1，同一个权重位，绝不与 usage 叠加成双罚）。
//
// 三态聚合：数值有效且未过期的窗口贡献 (1 - 剩余比例)；被权威完整清单证实存在但
// 数值/时效不可用的窗口贡献中性分 0.5；缺席或存在性未知的窗口直接省略，不给本没有
// 该窗口限制的账号凭空降权（Spec §7.2）。同窗口多桶取最小剩余（最紧桶决定压力）。
func (snap *AntigravityQuotaSnapshot) ComputeQuotaPressure(now time.Time) float64 {
	return snap.ComputeQuotaPressureForModel("", now)
}

// ComputeQuotaPressureForModel 是 ComputeQuotaPressure 的模型感知版本：只聚合 model
// 所在配额族的桶（另含无法归族的保守桶），Claude 周配额见底不再拖低同号 Gemini 请求
// 的排序，反之亦然。model 为空或无法归族时与整号口径一致。
func (snap *AntigravityQuotaSnapshot) ComputeQuotaPressureForModel(model string, now time.Time) float64 {
	if snap == nil {
		return neutralQuotaPressure
	}
	var scope antigravityQuotaBucketScope
	if strings.TrimSpace(model) != "" {
		scope, _ = antigravityQuotaScopeForModel(snap, model, true)
	}
	var total, weight float64
	for _, window := range quotaPressureWindows {
		value, present, exists := aggregateQuotaWindow(snap, window, scope, now)
		switch {
		case present:
			total += 1.0 - value
			weight += 1.0
		case exists:
			total += neutralQuotaPressure // 证实存在但数值/时效不可用：中性分（Spec §7.2）
			weight += 1.0
		}
		// absent / 存在性未知：省略，不给"无 5h 限制"的账号无端降权（Spec §7.2 修订）
	}
	if weight > 0 {
		return quantizeQuotaPressure(total / weight)
	}

	// groups 完全不可用（无 summary、窗口全部未识别、quota 关闭期间）：
	// 回退 models 级压力，只统计 presence 有效的条目，防止加权避让静默退化。
	var modelSum float64
	var modelCount int
	for _, m := range snap.Models {
		if m.FractionPresent && m.RemainingFraction != nil {
			v := *m.RemainingFraction
			if v >= 0.0 && v <= 1.0 && !math.IsNaN(v) && !math.IsInf(v, 0) {
				modelSum += v
				modelCount++
			}
		}
	}
	if modelCount > 0 {
		return quantizeQuotaPressure(1.0 - (modelSum / float64(modelCount)))
	}

	// 全无事实：全局中性先验。
	return neutralQuotaPressure
}

// antigravityQuotaPressureForScheduling 在账号读锁内取 raw 配额快照并就地折算压力分。
//
// SchemaVersion != 2（历史快照，presence 语义缺失、数值不可信）与完全无事实的快照一样
// 返回中性 0.5：软打分可以吃不准的数，但不能拿不可信的数去系统性压低某个账号
// （Spec §7.4）。压力分只在 Fresh 候选排序里消费，既有健康绑定复用分支不调用本方法，
// 因此已绑定健康会话天然豁免（Spec §7.3）。
//
// model 是本次请求的 public model，压力分只看它所在配额族；为空时按整号聚合。
func (a *Account) antigravityQuotaPressureForScheduling(model string, now time.Time) float64 {
	if a == nil {
		return neutralQuotaPressure
	}
	a.mu.RLock()
	defer a.mu.RUnlock()
	snap := a.AntigravityQuota
	if snap.SchemaVersion != 2 {
		return neutralQuotaPressure
	}
	return snap.ComputeQuotaPressureForModel(model, now)
}

// antigravityExpiryMinRemaining 是临期优先的存量下限：剩余不足 5% 的周配额冲刺价值
// 很小，反而会把新会话压到一个随时触顶的号上。
const antigravityExpiryMinRemaining = 0.05

// antigravityExpiryMinHours 是紧急度分母的下限，避免离重置只剩几分钟时分数发散。
const antigravityExpiryMinHours = 0.5

// antigravityExpiryUrgency 计算账号在 model 所在配额族上的临期紧急度。
//
// 只看周窗口：5h 窗口每 5 小时就重置一次，恒处于「临近重置」，计入会让临期档常驻；
// 周配额才是到期清零会造成实际浪费的那一份。同族多个周桶取最紧（剩余最少）的那个——
// 能用掉的量受最紧桶约束。
//
// 返回 urgent=true 当且仅当：模型可归族、存在新鲜（未超 TTL）且数值合法的周桶、
// 剩余 ≥ 5%、可信重置时刻在 (now, now+horizon] 内。此时 score = R / max(T小时, 0.5)：
// 剩得越多、离重置越近越优先。未知族的桶不参与——加分不能建立在猜测上。
func (a *Account) antigravityExpiryUrgency(model string, horizon time.Duration, now time.Time) (score float64, urgent bool) {
	if a == nil || horizon <= 0 {
		return 0, false
	}
	a.mu.RLock()
	defer a.mu.RUnlock()
	snap := &a.AntigravityQuota
	if snap.SchemaVersion != 2 {
		return 0, false
	}
	scope, known := antigravityQuotaScopeForModel(snap, model, false)
	if !known {
		return 0, false
	}
	remaining := math.Inf(1)
	var resetAt time.Time
	for _, g := range snap.Groups {
		for _, b := range g.Buckets {
			if !scope(b) {
				continue
			}
			w, ok := NormalizeAntigravityWindow(b.Window)
			if !ok || w != "weekly" || !b.FractionPresent || b.RemainingFraction == nil {
				continue
			}
			v := *b.RemainingFraction
			if v < 0.0 || v > 1.0 || math.IsNaN(v) || math.IsInf(v, 0) {
				continue
			}
			if !b.ObservedAt.IsZero() && now.After(b.ObservedAt.Add(time.Duration(b.TTLSeconds)*time.Second)) {
				continue // 观测过期：剩余比例可能早已被消耗，不能据此冲刺
			}
			reset, ok := ValidateResetTime(b.ResetTime, b.ObservedAt, w)
			if !ok || !reset.After(now) {
				continue
			}
			if v < remaining {
				remaining, resetAt = v, reset
			}
		}
	}
	if math.IsInf(remaining, 1) || remaining < antigravityExpiryMinRemaining {
		return 0, false
	}
	left := resetAt.Sub(now)
	if left > horizon {
		return 0, false
	}
	return quantizeQuotaPressure(remaining / math.Max(left.Hours(), antigravityExpiryMinHours)), true
}

// SelectionOptions 描述一次选号的调度上下文。
//
// Provider 是下游 Key 限定的上游渠道（database.UpstreamChannel*，空表示不指定/通用），
// Model 是请求的 public model，Policy 是用量闸门（Standard/Spark）。首绑与请求重试的
// Fresh 选号据此识别「这是一次 Antigravity 请求」，从而在 round_robin 下启用加权避险；
// 其余渠道与模式完全走既有选号逻辑，行为不变。
type SelectionOptions struct {
	Provider string // 例如 database.UpstreamChannelAntigravity；空表示不指定/通用
	Model    string // public model
	Policy   DispatchPolicy
}

// schedulerModeRoundRobin 是加权避险唯一生效的调度模式；remaining_quota / fill_first
// 保持既有用量优先语义，只继承冷却正确性修复。
const schedulerModeRoundRobin = "round_robin"

// isAntigravityProvider 报告 provider 是否指向 Antigravity 渠道。auth 侧的
// UpstreamAntigravity 与 database.UpstreamChannelAntigravity 同为 "antigravity"，
// 这里两者都接受，大小写不敏感。
func isAntigravityProvider(provider string) bool {
	provider = strings.TrimSpace(provider)
	if provider == "" {
		return false
	}
	return strings.EqualFold(provider, database.UpstreamChannelAntigravity) ||
		strings.EqualFold(provider, UpstreamAntigravity)
}

// antigravityBalancedSelectionEnabled 报告本次选号是否应走 Antigravity 加权避险：
// provider=antigravity、进程开关开启、且调度模式为 round_robin 时成立。
func (s *Store) antigravityBalancedSelectionEnabled(opts SelectionOptions) bool {
	if s == nil {
		return false
	}
	if !isAntigravityProvider(opts.Provider) {
		return false
	}
	if !GetAntigravityBalancedAffinity() {
		return false
	}
	return s.GetSchedulerMode() == schedulerModeRoundRobin
}

// fmix64 是 MurmurHash3 finalizer（也见于 SplitMix64）的 64 位雪崩混淆：三次
// 「右移 33 位亦或 + 大素数乘法」把输入每一位的差异扩散到全部 64 位输出。
//
// 纯算术实现，不依赖平台、字节序与编译器内联，任何 GOARCH 上恒得同一结果。
func fmix64(h uint64) uint64 {
	h ^= h >> 33
	h *= 0xff51afd7ed558ccd
	h ^= h >> 33
	h *= 0xc4ceb9fe1a85ec53
	h ^= h >> 33
	return h
}

// antigravityTieBreak 是加权选号同分时的确定性打散键：对 (亲和键, 账号ID) 做
// FNV-1a，再用 fmix64 做终值雪崩混淆。
//
// 同一亲和键在号池不变时恒得同一顺序（幂等），不同键把并列账号摊开。
//
// 终值混淆不是可选项：FNV-1a 只有「乘 1099511628211 后亦或」一步扩散，输入尾部
// 字节只影响输出的高位区间，低位比特几乎不混合。而这里的输入尾部是十进制 ASCII
// 的账号 ID——同批账号的 ID 是连续整数，后缀高度相似（"…1"、"…2"、"…10"、"…11"），
// 直接取 FNV-1a 最小值会把并列账号系统性地偏向某几个 ID 前缀/后缀，同分打散退化成
// 固定偏好。fmix64 之后每个输出比特都依赖全部输入比特，最小值选择才真正与账号身份
// 无关。
func antigravityTieBreak(key string, dbID int64) uint64 {
	hasher := fnv.New64a()
	_, _ = hasher.Write([]byte(key))
	_, _ = hasher.Write([]byte{':'})
	_, _ = hasher.Write([]byte(strconv.FormatInt(dbID, 10)))
	return fmix64(hasher.Sum64())
}

// selectAntigravityBalancedAccount 是首绑 / Fresh 选号的 Antigravity 加权避险实现。
//
// 候选按 (调度优先级, 健康档位) 从高到低分层，层内按
//
//	score = load + quotaPressure*0.50 + penalty*0.35   （load=occupied/limit，quotaPressure=额度压力分∈[0,1]）
//
// 升序打分（临期优先开启时，同层内请求模型所在配额族的周配额临期账号先于其余账号，
// 见 antigravityExpiryUrgency），同分用 (key, DBID) 哈希做确定性打散；从最高层开始逐个尝试取号，命中即返回，
// 整层都拿不下（并发被抢占 / 命中 cached cooldown）时继续下一层。它只影响「下一个新会话
// 优先选谁」，不改变任何冷却、绑定或并发语义。
//
// 第二个返回值 handled 表示加权路径是否已接管本次选号：provider=antigravity、进程开关
// 开启且调度模式为 round_robin 时成立。handled=false 表示本次不适用（调用方继续走既有
// spread / HRW / round-robin 逻辑）；handled=true 时调用方必须直接采用本函数的结果，
// 包括返回 nil 的情况——号池确实没有可用容量，不得再回退重扫。
//
// 单轮求值保护：本函数对每个候选账号的 filter(acc) 至多求值一次（快照循环内），取号循环
// 只做 cached cooldown 与 tryAcquire，不再触碰 filter，杜绝闭包重复触发与 Redis 读放大。
// 这也是 handled 存在的原因：一旦本函数已经消费过 filter，任何回退重扫都会二次求值。
func (s *Store) selectAntigravityBalancedAccount(key string, apiKeyID int64, exclude map[int64]bool, filter AccountFilter, opts SelectionOptions) (*Account, bool) {
	if !s.antigravityBalancedSelectionEnabled(opts) {
		return nil, false
	}
	filter = s.withUsableEgressFilter(filter)

	type candidate struct {
		acc               *Account
		schedulerPriority int64
		tierPriority      int
		limit             int64
		score             float64
		urgent            bool
		urgency           float64
		tie               uint64
	}

	maxConcurrency := s.maxConcurrency.Load()
	now := time.Now()
	penaltyWeight, usageWeight := AntigravityThrottleWeights()
	expiryEnabled, expiryHorizon := AntigravityExpiryPriority()

	accounts := s.accountSnapshotAccounts()
	candidates := make([]candidate, 0, len(accounts))
	for _, acc := range accounts {
		if exclude != nil && exclude[acc.DBID] {
			continue
		}
		if !acc.dispatchableForPolicy(opts.Policy) {
			continue
		}
		if !s.accountAllowedForAPIKey(acc, apiKeyID) {
			continue
		}
		// filter 单次求值：整个选号只在这里对 acc 调一次 filter。
		if filter != nil && !filter(acc) {
			continue
		}
		occupied := accountOccupiedRequests(acc)
		tier, _, _, limit := acc.schedulerSnapshotForPolicy(maxConcurrency, opts.Policy)
		if limit <= 0 || occupied >= limit {
			continue
		}
		load := float64(occupied) / float64(limit)
		// quotaPressure 整体替换原 usage 项（防双罚，Spec §7.1、验收 #19）：
		// 沿用同一 usageWeight 权重位，绝不与 usage 叠加。
		pressure := acc.antigravityQuotaPressureForScheduling(opts.Model, now)
		penalty := acc.AntigravityPenalty(now)
		score := load + pressure*usageWeight + penalty*penaltyWeight
		var urgency float64
		var urgent bool
		if expiryEnabled {
			urgency, urgent = acc.antigravityExpiryUrgency(opts.Model, expiryHorizon, now)
		}
		candidates = append(candidates, candidate{
			acc:               acc,
			schedulerPriority: acc.schedulerPriority(),
			tierPriority:      tierPriority(tier),
			limit:             limit,
			score:             score,
			urgent:            urgent,
			urgency:           urgency,
			tie:               antigravityTieBreak(key, acc.DBID),
		})
	}
	if len(candidates) == 0 {
		// 加权路径已接管但号池没有可用容量：交给调用方直接返回 nil 进入等待，
		// 不能再回退到会对同一批账号重扫 filter 的常规选号。
		return nil, true
	}

	// 一次排序同时确定层序与层内序：层按 (调度优先级, 健康档位) 降序，层内按 score 升序，
	// 同分用哈希 tie 升序做确定性打散。
	sort.Slice(candidates, func(i, j int) bool {
		left, right := candidates[i], candidates[j]
		if left.schedulerPriority != right.schedulerPriority {
			return left.schedulerPriority > right.schedulerPriority
		}
		if left.tierPriority != right.tierPriority {
			return left.tierPriority > right.tierPriority
		}
		// 临期优先（开关开启时）：同层内临期账号整体排在非临期之前，临期账号之间按紧急度
		// 降序。非临期账号保持原打分，远期号池的均衡不受影响。
		if left.urgent != right.urgent {
			return left.urgent
		}
		if left.urgent && left.urgency != right.urgency {
			return left.urgency > right.urgency
		}
		if left.score != right.score {
			return left.score < right.score
		}
		return left.tie < right.tie
	})

	// 逐层尝试：运营者设置的调度优先级与健康档位仍然高于低层账号，只有整层都拿不下才
	// 下沉到下一层，而不是直接放弃。
	for start := 0; start < len(candidates); {
		end := start + 1
		for end < len(candidates) &&
			candidates[end].schedulerPriority == candidates[start].schedulerPriority &&
			candidates[end].tierPriority == candidates[start].tierPriority {
			end++
		}
		// cached cooldown 只在取号循环里对「即将获取」的账号做一次验证，不做前置 N 次 Redis 读。
		for _, c := range candidates[start:end] {
			if s.accountHasBlockingCachedCooldown(c.acc, opts.Policy) {
				continue
			}
			if s.tryAcquireAccount(c.acc, c.limit, true) {
				return c.acc, true
			}
		}
		start = end
	}
	return nil, true
}

// NextForSessionWithDispatchGuardForProvider 是携带 SelectionOptions 的会话选号入口，
// 供 proxy 在已知请求渠道 / public model 时调用，使 Antigravity 首绑与 Fresh 重选走加权
// 避险。非 Antigravity 渠道或未开启时与 NextForSessionWithDispatchGuard 行为完全一致。
func (s *Store) NextForSessionWithDispatchGuardForProvider(opts SelectionOptions, key string, apiKeyID int64, exclude map[int64]bool, filter AccountFilter) (*Account, string, SessionAffinityGuard) {
	return s.nextForSessionWithFilter(key, apiKeyID, exclude, filter, false, opts)
}

// NextForContinuationWithDispatchForProvider 是续链请求的 SelectionOptions 变体。续链
// 只认绑定账号，加权避险不改变其绑定复用；opts 把 Policy 与渠道一起透传到底层选号——
// 绑定不存在（首次绑定）时续链会落到 Fresh 选号，Provider 缺失会让 Antigravity 请求
// 在重试链上丢掉加权避险上下文。
func (s *Store) NextForContinuationWithDispatchForProvider(opts SelectionOptions, key string, apiKeyID int64, exclude map[int64]bool, filter AccountFilter) (*Account, string) {
	account, proxyURL, _ := s.nextForSessionWithFilter(key, apiKeyID, exclude, filter, true, opts)
	return account, proxyURL
}

// WaitForDispatchAvailableForProvider 是 WaitForDispatchAvailable 的 SelectionOptions 变体，
// 让排队等待路径也能识别 Antigravity 请求并在恢复可用时走加权避险选号。
func (s *Store) WaitForDispatchAvailableForProvider(ctx context.Context, key string, timeout time.Duration, apiKeyID int64, exclude map[int64]bool, filter AccountFilter, opts SelectionOptions, preserveBinding bool, heartbeat ...SchedulerWaitHeartbeat) (*Account, string, SessionAffinityGuard, error) {
	return s.waitForSessionAvailableWithFilter(ctx, key, timeout, apiKeyID, exclude, filter, preserveBinding, opts, heartbeat...)
}
