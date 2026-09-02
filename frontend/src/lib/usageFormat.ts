// 长窗口(7d 槽)的显示标签。plus/pro 为周窗(7d),free/team plan 实为月窗(约 30 天),
// 标签写死 7d 会误导(issue #324)。优先用后端识别的窗口类型,其次按真实周期秒数推导,
// 都未知时回退 '7d'(与后端槽位命名一致)。
export function formatLongUsageWindowLabel(account: {
  usage_window_7d_kind?: string
  usage_window_7d_seconds?: number
}): string {
  if (account.usage_window_7d_kind === 'monthly') return '30d'
  const seconds = account.usage_window_7d_seconds
  if (typeof seconds === 'number' && Number.isFinite(seconds) && seconds > 0) {
    const days = Math.round(seconds / 86_400)
    if (days >= 1) return `${days}d`
    const hours = Math.round(seconds / 3_600)
    if (hours >= 1) return `${hours}h`
  }
  return '7d'
}

export function formatUsageNumber(
  value?: number | null,
  showFullNumbers = false,
  locale?: Intl.LocalesArgument,
): string {
  if (value === undefined || value === null) return '0'

  const numericValue = Number(value)
  if (!Number.isFinite(numericValue)) return '0'

  const roundedValue = Math.round(numericValue)
  if (showFullNumbers) return roundedValue.toLocaleString(locale)

  const absValue = Math.abs(numericValue)
  const units = [
    { value: 1_000_000_000_000, suffix: 'T' },
    { value: 1_000_000_000, suffix: 'B' },
    { value: 1_000_000, suffix: 'M' },
    { value: 1_000, suffix: 'K' },
  ]
  const unit = units.find((item) => absValue >= item.value)
  if (!unit) return roundedValue.toLocaleString(locale)

  const scaled = numericValue / unit.value
  const fractionDigits = Math.abs(scaled) >= 100 ? 0 : Math.abs(scaled) >= 10 ? 1 : 2
  const compact = scaled
    .toFixed(fractionDigits)
    .replace(/\.0+$/, '')
    .replace(/(\.\d*?)0+$/, '$1')

  return `${compact}${unit.suffix}`
}

export function needsUsageReload(account: {
  status?: string
  usage_percent_5h?: number | null
  usage_percent_7d?: number | null
  claude_api?: boolean
  claude_usage_probe_at?: string | null
  claude_usage_probe_error?: string | null
}): boolean {
  if (account.status !== 'active' && account.status !== 'ready') return false

  const has5h =
    account.usage_percent_5h !== null && account.usage_percent_5h !== undefined
  const has7d =
    account.usage_percent_7d !== null && account.usage_percent_7d !== undefined
  // Claude's native Messages probe can legitimately return no quota headers.
  // A successful probe is still a completed sample and must not trigger an
  // endless page refresh loop.
  if (hasSuccessfulClaudeProbe(account)) return false
  return !has5h && !has7d
}

type AccountStatusSource = {
  status?: string | null
  openai_responses_api?: boolean
  grok_api?: boolean
  claude_api?: boolean
  claude_usage_probe_at?: string | null
  claude_usage_probe_error?: string | null
  usage_percent_5h?: number | null
  usage_percent_7d?: number | null
}

function hasSuccessfulClaudeProbe(account: AccountStatusSource): boolean {
  return Boolean(
    account.claude_api &&
      account.claude_usage_probe_at?.trim() &&
      !account.claude_usage_probe_error?.trim(),
  )
}

export function isUnsampledQuotaAccount(account: AccountStatusSource): boolean {
  const status = (account.status || '').toLowerCase()
  if (
    status === 'unauthorized' ||
    status === 'error' ||
    account.openai_responses_api ||
    account.grok_api
  ) {
    return false
  }
  // k12 等 team 型工作区可能只返回 5h 窗口：任一窗口有数据即算已采样。
  const has7d =
    typeof account.usage_percent_7d === 'number' &&
    Number.isFinite(account.usage_percent_7d)
  const has5h =
    typeof account.usage_percent_5h === 'number' &&
    Number.isFinite(account.usage_percent_5h)
  if (hasSuccessfulClaudeProbe(account)) return false
  return !has7d && !has5h
}

export function getAccountStatusBadgeStatus(account: AccountStatusSource): string {
  const status = account.status || 'unknown'
  if (status === 'overload_paused') return 'active'
  const key = status.toLowerCase()
  if ((key === 'active' || key === 'ready') && isUnsampledQuotaAccount(account)) {
    return 'unsampled'
  }
  return status
}

// 官方结算通常在账号产生请求后的次日才出数。导入未满一天就拉上游只会空转。
export const officialCostMinAccountAgeMs = 24 * 60 * 60 * 1000

export function isOfficialCostHiddenAccount(account: {
  status?: string | null
}): boolean {
  const status = (account.status || '').toLowerCase()
  return status === 'unauthorized' || status === 'error'
}

export function isOfficialCostTooNew(
  account: { created_at?: string | null },
  now = Date.now(),
): boolean {
  if (!account.created_at) return false
  const created = Date.parse(account.created_at)
  if (!Number.isFinite(created)) return false
  return now - created < officialCostMinAccountAgeMs
}

export function supportsOfficialUsage(account: {
  access_token_type?: string | null
  openai_responses_api?: boolean
  grok_api?: boolean
  claude_api?: boolean
}): boolean {
  if (account.openai_responses_api || account.grok_api || account.claude_api) return false
  return (account.access_token_type || '').trim().toLowerCase() !== 'codex_at'
}

export function officialUsdValue(account: {
  official_usd?: number | null
  official_usd_7d?: number | null
}): number | null {
  if (typeof account.official_usd === 'number') return account.official_usd
  if (typeof account.official_usd_7d === 'number') return account.official_usd_7d
  return null
}

// Codex OAuth 账号的官方结算成本来自本地快照。列表打开时快照经常还是空的，
// 需要重拉 page-stats 直到回补完成；codex_at、中转和 Grok 没有该链路，不要空转。
// official_usage_synced 表示后端已成功同步过但上游没有数据（官方统计有
// 滞后），这时继续重拉也不会有结果，交给后台小时级探针即可。
export function needsOfficialCostReload(account: {
  status?: string | null
  created_at?: string | null
  access_token_type?: string | null
  openai_responses_api?: boolean
  grok_api?: boolean
  claude_api?: boolean
  official_usd?: number | null
  official_usd_7d?: number | null
  official_usage_synced?: boolean
}): boolean {
  if (!supportsOfficialUsage(account)) return false
  if (isOfficialCostHiddenAccount(account) || isOfficialCostTooNew(account)) return false
  if (account.official_usage_synced) return false
  return officialUsdValue(account) === null
}

// 列表「官方结算」跟官方统计页同一次同步对齐：把刚刷下来的按天快照全部累加。
// windowDays > 0 时以上游最新一天为窗口终点截取，避免浏览器时区错一天。
export function officialUsdFromDailyItems(
  items: Array<{ day?: string | null; usd?: number | null }>,
  windowDays = 0,
): number | null {
  if (!Array.isArray(items) || items.length === 0) return null
  let cutoffDay = ''
  if (windowDays > 0) {
    const days = items
      .map((item) => (item.day || '').trim())
      .filter((day) => /^\d{4}-\d{2}-\d{2}$/.test(day))
      .sort()
    const latest = days[days.length - 1]
    if (!latest) return null
    const latestDate = new Date(`${latest}T00:00:00Z`)
    if (Number.isNaN(latestDate.getTime())) return null
    const cutoff = new Date(latestDate)
    cutoff.setUTCDate(cutoff.getUTCDate() - (windowDays - 1))
    cutoffDay = cutoff.toISOString().slice(0, 10)
  }
  let usd = 0
  let any = false
  for (const item of items) {
    const day = (item.day || '').trim()
    if (cutoffDay && day < cutoffDay) continue
    const value = Number(item.usd)
    if (!Number.isFinite(value)) continue
    usd += value
    any = true
  }
  return any ? usd : null
}
