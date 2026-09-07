export function formatSessionUsageDuration(seconds?: number | null): string {
  if (seconds == null || !Number.isFinite(seconds) || seconds < 0) return '-'
  const total = Math.round(seconds)
  if (seconds > 0 && total === 0) return '<1s'
  const hours = Math.floor(total / 3600)
  const minutes = Math.floor((total % 3600) / 60)
  const remainder = total % 60
  const parts: string[] = []
  if (hours > 0) parts.push(`${hours}h`)
  if (minutes > 0) parts.push(`${minutes}m`)
  if (remainder > 0 || parts.length === 0) parts.push(`${remainder}s`)
  return parts.join(' ')
}
