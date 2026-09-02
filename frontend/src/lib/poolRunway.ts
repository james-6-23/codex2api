/**
 * 号池可支撑时间（Pool Runway）与限流压力预测。
 * 从 AccountRateLimitRecoveryChart 抽出，供账号页与 Dashboard 共用。
 *
 * - 负载口径：当前 RPM 下何时供应不足 / 高压
 * - 额度口径：按 usage% 线性外推批量限流风险
 * - 展示：剩余 x 小时 / 1 天+（issue #383 产品化）
 */

import type { AccountPressureForecastAnalysis, AccountRow } from '../types'

export type RecoveryWindow = '5h' | '7d'
export type RiskLevel = 'low' | 'medium' | 'high'
export type RunwayKind = 'critical' | 'hours' | 'day_plus' | 'stable' | 'unknown'
export type RunwayState = 'shortage' | 'high' | 'limit_risk' | 'stable'
export type RunwayAdviceCode = 'add_accounts' | 'reduce_load' | 'ok' | 'low_confidence' | 'critical'

export interface PressureForecast {
  sampled: number
  threshold: number
  predictedAt: number | null
  predictedCount: number
  unknown: number
  rpm: number
  effectiveRpmLimit: number
  rpmPressure: number | null
  activePressure: number
  rateLimitPressure: number
  dispatchableAccounts: number
  avgConcurrency: number
  highPressureAt: number | null
  supplyShortageAt: number | null
  riskLevel: RiskLevel
  confidence: number
}

export interface PoolRunway {
  kind: RunwayKind
  state: RunwayState
  remainingMs: number | null
  /** 剩余整小时数（ceil）；kind=hours 时有意义 */
  remainingHours: number | null
  riskLevel: RiskLevel
  pressureAt: number | null
  windowKey: RecoveryWindow
  adviceCode: RunwayAdviceCode
  /** 建议补充的可调度账号数（0 表示不需要） */
  suggestedAddAccounts: number
  forecast: PressureForecast
  lowConfidence: boolean
}

interface SupplyEvent {
  at: number
  concurrency: number
  delta: 1 | -1
  paired?: boolean
}

interface SupplyPressurePoint {
  highPressureAt: number | null
  supplyShortageAt: number | null
}

const RPM_PER_CONCURRENCY_SLOT_DEFAULT = 6
const RPM_PER_CONCURRENCY_SLOT_MIN = 1
const RPM_PER_CONCURRENCY_SLOT_MAX = 30
const RATE_LIMIT_SATURATION_FRACTION = 0.3
const RECENT_RATE_LIMIT_WINDOW_MS = 60 * 60_000
const BULK_LIMIT_RATIO = 0.3
const BULK_LIMIT_MIN_COUNT = 3
const PRESSURE_FACTOR_MAX = 2.5
const PRESSURE_BOOST_DOMINANT = 1.0
const PRESSURE_BOOST_SECONDARY = 0.5
const PRESSURE_THRESHOLD_RPM = 0.75
const PRESSURE_THRESHOLD_ACTIVE = 0.75
const LOW_CONFIDENCE_THRESHOLD = 0.4
const BURN_MIN_ELAPSED_RATIO = 0.05
const SOON_WINDOW_RATIO = 0.2
const HOUR_MS = 60 * 60_000
const DAY_MS = 24 * HOUR_MS

export function estimatePressureForecast(
  accounts: AccountRow[],
  windowKey: RecoveryWindow,
  nowMs: number,
  currentRpm: number,
  rpmLimit: number,
  avgDurationMs: number,
): PressureForecast {
  const defaultWindowMs = getDefaultWindowMs(windowKey)
  const rpmPerSlot = getRpmPerSlot(avgDurationMs)
  const projectedLimitTimes: number[] = []
  const supplyEvents: SupplyEvent[] = []
  const dispatchableAccounts = accounts.filter((account) => isInSupplyPool(account, windowKey))
  const totalConcurrency = dispatchableAccounts.reduce((sum, account) => sum + getEffectiveConcurrency(account), 0)
  const avgConcurrency = dispatchableAccounts.length > 0 ? totalConcurrency / dispatchableAccounts.length : 0
  const activeRequests = dispatchableAccounts.reduce((sum, account) => sum + normalizeNumber(account.active_requests), 0)
  const activePressure = totalConcurrency > 0 ? clamp(activeRequests / totalConcurrency, 0, 3) : 0

  const currentlyRateLimited = accounts.filter((account) => {
    const status = (account.status || '').toLowerCase()
    if (status === 'unauthorized') return false
    if (account.enabled === false) return false
    return isWindowRateLimitLike(account, windowKey)
  })
  const recentlyRateLimitedFromPool = dispatchableAccounts.filter((account) => {
    if (!account.last_rate_limited_at) return false
    const ts = new Date(account.last_rate_limited_at).getTime()
    return Number.isFinite(ts) && ts > 0 && nowMs - ts <= RECENT_RATE_LIMIT_WINDOW_MS
  })
  const rateLimitedSignalDenominator = dispatchableAccounts.length + currentlyRateLimited.length
  const rateLimitedSignalNumerator = recentlyRateLimitedFromPool.length + currentlyRateLimited.length
  const recentRateLimitedFraction = rateLimitedSignalDenominator > 0
    ? rateLimitedSignalNumerator / rateLimitedSignalDenominator
    : 0
  const rateLimitPressure = clamp(recentRateLimitedFraction / RATE_LIMIT_SATURATION_FRACTION, 0, 1)
  const normalizedRpm = normalizeNumber(currentRpm)
  const configuredRpmLimit = normalizeNumber(rpmLimit)
  const concurrencyRpmLimit = totalConcurrency > 0
    ? Math.max(1, Math.round(totalConcurrency * rpmPerSlot))
    : 0
  const effectiveRpmLimit = getEffectiveRpmLimit(configuredRpmLimit, concurrencyRpmLimit)
  const rpmPressure = effectiveRpmLimit > 0 ? normalizedRpm / effectiveRpmLimit : null
  const pressureFactor = getPressureFactor(rpmPressure, activePressure, rateLimitPressure)
  let sampled = 0
  let unknown = 0

  for (const account of accounts) {
    if (!hasBurnPrediction(account, windowKey)) {
      continue
    }
    const inSupply = isInSupplyPool(account, windowKey)
    const concurrency = getEffectiveConcurrency(account)
    const usage = windowKey === '5h' ? account.usage_percent_5h : account.usage_percent_7d
    const rawResetAt = windowKey === '5h' ? account.reset_5h_at : account.reset_7d_at
    const accountWindowMs = getAccountWindowMs(account, windowKey)
    const burnMinElapsedMs = accountWindowMs * BURN_MIN_ELAPSED_RATIO
    const knownResetAt = futureTimestamp(rawResetAt, nowMs)
    const resetAt = knownResetAt ?? (nowMs + accountWindowMs)

    if (!inSupply) {
      if (knownResetAt) {
        supplyEvents.push({ at: knownResetAt, concurrency, delta: 1 })
      }
      if (typeof usage !== 'number' || !Number.isFinite(usage)) {
        unknown += 1
      }
      continue
    }

    if (typeof usage !== 'number' || !Number.isFinite(usage)) {
      unknown += 1
      continue
    }

    sampled += 1
    const usedPercent = clamp(usage, 0, 100)
    if (usedPercent >= 100) {
      projectedLimitTimes.push(nowMs)
      supplyEvents.push({ at: nowMs, concurrency, delta: -1 })
      supplyEvents.push({ at: resetAt, concurrency, delta: 1, paired: true })
      continue
    }

    const windowStartAt = resetAt - accountWindowMs
    const elapsedMs = Math.max(60_000, nowMs - windowStartAt)
    if (elapsedMs < burnMinElapsedMs) {
      continue
    }
    const burnRatePerMs = usedPercent / elapsedMs
    if (burnRatePerMs <= 0) {
      unknown += 1
      continue
    }
    const predictedAt = nowMs + ((100 - usedPercent) / burnRatePerMs)
    if (Number.isFinite(predictedAt) && predictedAt <= resetAt) {
      projectedLimitTimes.push(predictedAt)
      supplyEvents.push({ at: predictedAt, concurrency, delta: -1 })
      supplyEvents.push({ at: resetAt, concurrency, delta: 1, paired: true })
    }
  }

  projectedLimitTimes.sort((a, b) => a - b)
  supplyEvents.sort((a, b) => a.at - b.at)
  const supplyPressurePoint = estimateSupplyPressurePoint(
    supplyEvents,
    normalizedRpm,
    configuredRpmLimit,
    totalConcurrency,
    nowMs,
    rpmPerSlot,
  )
  const minAccountsForRpm = (normalizedRpm > 0 && avgConcurrency > 0)
    ? Math.ceil(normalizedRpm / (avgConcurrency * rpmPerSlot))
    : 0
  const capacityThreshold = minAccountsForRpm > 0 && dispatchableAccounts.length > 0
    ? Math.max(BULK_LIMIT_MIN_COUNT, dispatchableAccounts.length - minAccountsForRpm)
    : Math.max(BULK_LIMIT_MIN_COUNT, Math.ceil(sampled * BULK_LIMIT_RATIO))
  const threshold = sampled > 0 ? Math.min(sampled, capacityThreshold) : 0
  const quotaPredictedAt = findBulkLimitTime(supplyEvents, threshold)
  const predictedAt = quotaPredictedAt
    ? nowMs + ((quotaPredictedAt - nowMs) / pressureFactor)
    : null
  const totalEligible = sampled + unknown
  const confidence = totalEligible > 0 ? sampled / totalEligible : 0
  const riskLevel = getForecastRiskLevel(
    predictedAt,
    supplyPressurePoint.highPressureAt,
    supplyPressurePoint.supplyShortageAt,
    nowMs,
    defaultWindowMs,
    rpmPressure,
    activePressure,
    rateLimitPressure,
    confidence,
  )

  return {
    sampled,
    threshold,
    predictedAt,
    predictedCount: quotaPredictedAt
      ? projectedLimitTimes.filter((item) => item <= quotaPredictedAt).length
      : projectedLimitTimes.length,
    unknown,
    rpm: normalizedRpm,
    effectiveRpmLimit,
    rpmPressure,
    activePressure,
    rateLimitPressure,
    dispatchableAccounts: dispatchableAccounts.length,
    avgConcurrency,
    highPressureAt: supplyPressurePoint.highPressureAt,
    supplyShortageAt: supplyPressurePoint.supplyShortageAt,
    riskLevel,
    confidence,
  }
}

/** 从预测结果生成运维可读的 Runway（剩余 x 小时 / 1 天+）。 */
export function buildPoolRunway(
  forecast: PressureForecast,
  nowMs: number,
  windowKey: RecoveryWindow,
): PoolRunway {
  const pressureAt = forecast.supplyShortageAt ?? forecast.highPressureAt ?? forecast.predictedAt
  const remainingMs = pressureAt != null ? Math.max(0, pressureAt - nowMs) : null
  const lowConfidence = (forecast.sampled + forecast.unknown) > 0
    && forecast.confidence < LOW_CONFIDENCE_THRESHOLD

  let state: RunwayState = 'stable'
  if (forecast.supplyShortageAt != null) state = 'shortage'
  else if (forecast.highPressureAt != null) state = 'high'
  else if (forecast.predictedAt != null) state = 'limit_risk'

  let kind: RunwayKind
  let remainingHours: number | null = null
  if (state === 'shortage' && (remainingMs == null || remainingMs < HOUR_MS)) {
    kind = 'critical'
  } else if (remainingMs == null) {
    kind = forecast.riskLevel === 'low' && state === 'stable' ? 'stable' : 'unknown'
  } else if (remainingMs >= DAY_MS) {
    kind = 'day_plus'
  } else if (remainingMs < HOUR_MS) {
    kind = 'critical'
    remainingHours = 1
  } else {
    kind = 'hours'
    remainingHours = Math.max(1, Math.ceil(remainingMs / HOUR_MS))
  }

  const suggestedAddAccounts = suggestAddAccounts(forecast)
  let adviceCode: RunwayAdviceCode = 'ok'
  if (kind === 'critical' || state === 'shortage') {
    adviceCode = suggestedAddAccounts > 0 ? 'critical' : 'reduce_load'
  } else if (lowConfidence) {
    adviceCode = 'low_confidence'
  } else if (suggestedAddAccounts > 0 && (kind === 'hours' || forecast.riskLevel !== 'low')) {
    adviceCode = 'add_accounts'
  } else if ((forecast.rpmPressure ?? 0) >= 0.9) {
    adviceCode = 'reduce_load'
  }

  return {
    kind,
    state,
    remainingMs,
    remainingHours,
    riskLevel: forecast.riskLevel,
    pressureAt,
    windowKey,
    adviceCode,
    suggestedAddAccounts,
    forecast,
    lowConfidence,
  }
}

/**
 * 计算 7d（主）与可选 5h，取更紧迫的压力时刻作为号池 runway。
 * Dashboard 默认用此函数。
 */
export function selectPoolRunway(
  accounts: AccountRow[],
  nowMs: number,
  currentRpm: number,
  rpmLimit: number,
  avgDurationMs: number,
): PoolRunway {
  const primary = buildPoolRunway(
    estimatePressureForecast(accounts, '7d', nowMs, currentRpm, rpmLimit, avgDurationMs),
    nowMs,
    '7d',
  )

  const has5hSample = accounts.some(
    (a) => typeof a.usage_percent_5h === 'number' && Number.isFinite(a.usage_percent_5h),
  )
  if (!has5hSample) {
    return primary
  }

  const secondary = buildPoolRunway(
    estimatePressureForecast(accounts, '5h', nowMs, currentRpm, rpmLimit, avgDurationMs),
    nowMs,
    '5h',
  )

  // 优先有明确压力时刻的；都有则取更早的
  const primaryAt = primary.pressureAt
  const secondaryAt = secondary.pressureAt
  if (primaryAt == null && secondaryAt == null) {
    return primary.riskLevel === 'high' || primary.riskLevel === 'medium' ? primary : secondary.riskLevel !== 'low' ? secondary : primary
  }
  if (primaryAt == null) return secondary
  if (secondaryAt == null) return primary
  return secondaryAt < primaryAt ? secondary : primary
}

/** Convert the fixed-size server aggregate into the existing runway model. */
export function pressureForecastFromAnalysis(
  source: AccountPressureForecastAnalysis,
): PressureForecast {
  return {
    sampled: source.sampled,
    threshold: source.threshold,
    predictedAt: source.predicted_at,
    predictedCount: source.predicted_count,
    unknown: source.unknown,
    rpm: source.rpm,
    effectiveRpmLimit: source.effective_rpm_limit,
    rpmPressure: source.rpm_pressure,
    activePressure: source.active_pressure,
    rateLimitPressure: source.rate_limit_pressure,
    dispatchableAccounts: source.dispatchable_accounts,
    avgConcurrency: source.avg_concurrency,
    highPressureAt: source.high_pressure_at,
    supplyShortageAt: source.supply_shortage_at,
    riskLevel: source.risk_level,
    confidence: source.confidence,
  }
}

/** Dashboard variant that selects the tighter cached 5h/7d forecast. */
export function selectPoolRunwayFromAnalysis(
  forecasts: Record<RecoveryWindow, AccountPressureForecastAnalysis>,
  nowMs: number,
): PoolRunway {
  const primary = buildPoolRunway(
    pressureForecastFromAnalysis(forecasts['7d']),
    nowMs,
    '7d',
  )
  const fiveHourForecast = forecasts['5h']
  if (fiveHourForecast.sampled <= 0 && fiveHourForecast.unknown <= 0) {
    return primary
  }
  const secondary = buildPoolRunway(
    pressureForecastFromAnalysis(fiveHourForecast),
    nowMs,
    '5h',
  )
  if (primary.pressureAt == null && secondary.pressureAt == null) {
    return primary.riskLevel === 'high' || primary.riskLevel === 'medium'
      ? primary
      : secondary.riskLevel !== 'low'
        ? secondary
        : primary
  }
  if (primary.pressureAt == null) return secondary
  if (secondary.pressureAt == null) return primary
  return secondary.pressureAt < primary.pressureAt ? secondary : primary
}

function suggestAddAccounts(forecast: PressureForecast): number {
  if (forecast.dispatchableAccounts <= 0) return 0
  const rpmPerSlot = getRpmPerSlot(0) // use default; advice is approximate
  const avgConc = forecast.avgConcurrency > 0 ? forecast.avgConcurrency : 1
  const minForRpm = forecast.rpm > 0
    ? Math.ceil(forecast.rpm / (avgConc * rpmPerSlot))
    : 0
  // 维持当前 RPM 需要 minForRpm；再留 20% 缓冲
  const target = minForRpm > 0 ? Math.ceil(minForRpm * 1.2) : 0
  if (target <= forecast.dispatchableAccounts) {
    // 额度燃尽路径：按 threshold 提示补齐
    if (forecast.predictedAt != null && forecast.riskLevel !== 'low') {
      return Math.max(1, Math.min(5, Math.ceil(forecast.dispatchableAccounts * 0.15)))
    }
    return 0
  }
  return Math.min(20, target - forecast.dispatchableAccounts)
}

function findBulkLimitTime(events: SupplyEvent[], threshold: number): number | null {
  if (threshold <= 0) return null
  let exhausted = 0
  for (const event of events) {
    if (event.delta === -1) {
      exhausted += 1
      if (exhausted >= threshold) return event.at
    } else if (event.paired) {
      exhausted = Math.max(0, exhausted - 1)
    }
  }
  return null
}

export function isInSupplyPool(account: AccountRow, windowKey: RecoveryWindow): boolean {
  const status = (account.status || '').toLowerCase()
  if (status === 'unauthorized') return false
  if (account.enabled === false) return false
  if (isWindowRateLimitLike(account, windowKey)) return false
  return true
}

/** 5h 为可选窗口：无 usage 快照则不参与燃尽预测（issue #382）。 */
export function hasBurnPrediction(account: AccountRow, windowKey: RecoveryWindow): boolean {
  const status = (account.status || '').toLowerCase()
  if (status === 'unauthorized') return false
  if (account.openai_responses_api) return false
  if (windowKey === '5h') {
    // Claude's rolling 5h window uses Anthropic plan keys (max-5x/max-20x,
    // enterprise, etc.), which are intentionally separate from Codex plans.
    if (account.claude_api ? !isClaudeUsagePlan(account.plan_type) : !isPremiumUsagePlan(account.plan_type)) return false
    return typeof account.usage_percent_5h === 'number' && Number.isFinite(account.usage_percent_5h)
  }
  return true
}

export function isWindowRateLimitLike(account: AccountRow, windowKey: RecoveryWindow): boolean {
  if (windowKey === '5h') {
    const eligible = account.claude_api ? isClaudeUsagePlan(account.plan_type) : isPremiumUsagePlan(account.plan_type)
    return (eligible && isUsageExhausted(account.usage_percent_5h)) || isShortRateLimitLike(account)
  }
  const status = (account.status || '').toLowerCase()
  const reason = (account.cooldown_reason || '').toLowerCase()
  return isUsageExhausted(account.usage_percent_7d) ||
    status === 'usage_exhausted' ||
    status === 'rate_limited_7d' ||
    reason === 'rate_limited_7d'
}

function isShortRateLimitLike(account: AccountRow): boolean {
  const status = (account.status || '').toLowerCase()
  const reason = (account.cooldown_reason || '').toLowerCase()
  if (status === 'rate_limited' || status === 'responses_rate_limited' || status === 'rate_limited_5h' || status === 'cooldown') {
    return true
  }
  if (reason === 'rate_limited' || reason === 'responses_rate_limited' || reason === 'rate_limited_5h') {
    return true
  }
  return false
}

function getEffectiveConcurrency(account: AccountRow): number {
  const value = account.dynamic_concurrency_limit ??
    account.base_concurrency_effective ??
    account.base_concurrency_override ??
    1
  return clamp(normalizeNumber(value), 1, 50)
}

function getRpmPerSlot(avgDurationMs: number): number {
  if (!avgDurationMs || avgDurationMs <= 0 || !Number.isFinite(avgDurationMs)) {
    return RPM_PER_CONCURRENCY_SLOT_DEFAULT
  }
  return clamp(60_000 / avgDurationMs, RPM_PER_CONCURRENCY_SLOT_MIN, RPM_PER_CONCURRENCY_SLOT_MAX)
}

function getEffectiveRpmLimit(configuredRpmLimit: number, concurrencyRpmLimit: number): number {
  if (concurrencyRpmLimit <= 0) {
    return 0
  }
  if (configuredRpmLimit > 0 && concurrencyRpmLimit > 0) {
    return Math.min(configuredRpmLimit, concurrencyRpmLimit)
  }
  return concurrencyRpmLimit
}

function getPressureFactor(rpmPressure: number | null, activePressure: number, rateLimitPressure: number): number {
  const rpmBoost = Math.max(0, (rpmPressure ?? 0) - PRESSURE_THRESHOLD_RPM)
  const activeBoost = Math.max(0, activePressure - PRESSURE_THRESHOLD_ACTIVE)
  const rateLimitBoost = rateLimitPressure
  const boosts = [rpmBoost, activeBoost, rateLimitBoost].sort((a, b) => b - a)
  const composite = boosts[0] * PRESSURE_BOOST_DOMINANT + boosts[1] * PRESSURE_BOOST_SECONDARY
  return clamp(1 + composite, 1, PRESSURE_FACTOR_MAX)
}

function estimateSupplyPressurePoint(
  events: SupplyEvent[],
  currentRpm: number,
  configuredRpmLimit: number,
  totalConcurrency: number,
  nowMs: number,
  rpmPerSlot: number,
): SupplyPressurePoint {
  if (currentRpm <= 0) {
    return { highPressureAt: null, supplyShortageAt: null }
  }

  let remainingConcurrency = totalConcurrency
  let capacity = getEffectiveRpmLimit(configuredRpmLimit, Math.round(remainingConcurrency * rpmPerSlot))
  let pressure = capacity > 0 ? currentRpm / capacity : Number.POSITIVE_INFINITY
  let highPressureAt = pressure >= 0.9 ? nowMs : null
  let supplyShortageAt = pressure >= 1 ? nowMs : null

  for (const event of events) {
    remainingConcurrency = Math.max(0, remainingConcurrency + event.delta * event.concurrency)
    capacity = getEffectiveRpmLimit(configuredRpmLimit, Math.round(remainingConcurrency * rpmPerSlot))
    pressure = capacity > 0 ? currentRpm / capacity : Number.POSITIVE_INFINITY

    if (!highPressureAt && pressure >= 0.9) {
      highPressureAt = event.at
    }
    if (!supplyShortageAt && pressure >= 1) {
      supplyShortageAt = event.at
      break
    }
  }

  return { highPressureAt, supplyShortageAt }
}

export function getDefaultWindowMs(windowKey: RecoveryWindow): number {
  return windowKey === '5h' ? 5 * HOUR_MS : 7 * DAY_MS
}

/** 按账号真实长窗口长度（月窗 ~30d / 周窗 7d），避免 team 被当成 7d。 */
export function getAccountWindowMs(account: AccountRow, windowKey: RecoveryWindow): number {
  if (windowKey === '5h') return 5 * HOUR_MS
  const sec = account.usage_window_7d_seconds
  if (typeof sec === 'number' && Number.isFinite(sec) && sec > 0) {
    return sec * 1000
  }
  if (account.usage_window_7d_kind === 'monthly') {
    return 30 * DAY_MS
  }
  return 7 * DAY_MS
}

function getForecastRiskLevel(
  predictedAt: number | null,
  highPressureAt: number | null,
  supplyShortageAt: number | null,
  nowMs: number,
  windowMs: number,
  rpmPressure: number | null,
  activePressure: number,
  rateLimitPressure: number,
  confidence: number,
): RiskLevel {
  const soonWindowMs = windowMs * SOON_WINDOW_RATIO
  const burnSignalReliable = confidence >= LOW_CONFIDENCE_THRESHOLD
  if (
    (supplyShortageAt && supplyShortageAt - nowMs <= soonWindowMs) ||
    (burnSignalReliable && predictedAt && predictedAt - nowMs <= soonWindowMs) ||
    (rpmPressure ?? 0) >= 1 ||
    activePressure >= 0.9 ||
    rateLimitPressure >= 0.8
  ) {
    return 'high'
  }
  if (highPressureAt || (burnSignalReliable && predictedAt) || (rpmPressure ?? 0) >= 0.7 || activePressure >= 0.7 || rateLimitPressure >= 0.4) {
    return 'medium'
  }
  return 'low'
}

function futureTimestamp(value: string | undefined, nowMs: number): number | null {
  if (!value) return null
  const timestamp = new Date(value).getTime()
  if (!Number.isFinite(timestamp) || timestamp <= nowMs) {
    return null
  }
  return timestamp
}

function isUsageExhausted(value?: number | null): boolean {
  return typeof value === 'number' && Number.isFinite(value) && value >= 100
}

function clamp(value: number, min: number, max: number): number {
  return Math.min(max, Math.max(min, value))
}

function normalizeNumber(value?: number | null): number {
  return typeof value === 'number' && Number.isFinite(value) ? value : 0
}

function normalizePlanType(planType?: string): string {
  const raw = (planType || '').toLowerCase().trim()
  if (raw === 'prolite' || raw === 'pro_lite' || raw === 'pro-lite') return 'pro'
  return raw
}

export function isPremiumUsagePlan(planType?: string): boolean {
  return ['plus', 'pro', 'team', 'teamplus', 'k12', 'edu', 'education', 'go'].includes(normalizePlanType(planType))
}

/** Claude OAuth 的订阅档位，按 profile 归一化后的 plan_type 匹配。 */
export function isClaudeUsagePlan(planType?: string): boolean {
  const normalized = normalizePlanType(planType)
  return ['claude', 'pro', 'max', 'max-5x', 'max-20x', 'team', 'enterprise', 'business'].includes(normalized) || normalized.startsWith('claude-')
}

export const POOL_RUNWAY_LOW_CONFIDENCE_THRESHOLD = LOW_CONFIDENCE_THRESHOLD
