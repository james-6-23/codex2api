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
