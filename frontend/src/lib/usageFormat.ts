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
}): boolean {
  if (account.status !== 'active' && account.status !== 'ready') return false

  const has5h =
    account.usage_percent_5h !== null && account.usage_percent_5h !== undefined
  const has7d =
    account.usage_percent_7d !== null && account.usage_percent_7d !== undefined
  return !has5h && !has7d
}
