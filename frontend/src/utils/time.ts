export interface RelativeTimeOptions {
  variant?: 'long' | 'compact'
  includeSeconds?: boolean
  fallback?: string
}

export function formatRelativeTime(dateStr?: string | null, options: RelativeTimeOptions = {}): string {
  const {
    variant = 'long',
    includeSeconds = false,
    fallback = '-',
  } = options

  if (!dateStr) {
    return fallback
  }

  const timestamp = new Date(dateStr).getTime()
  if (Number.isNaN(timestamp)) {
    return fallback
  }

  const diff = Math.max(0, Date.now() - timestamp)
  const seconds = Math.floor(diff / 1000)

  if (includeSeconds && seconds < 60) {
    return variant === 'compact' ? `${seconds}s 前` : `${seconds} 秒前`
  }

  const minutes = Math.floor(seconds / 60)
  if (minutes < 1) {
    return '刚刚'
  }

  if (minutes < 60) {
    return variant === 'compact' ? `${minutes}m 前` : `${minutes} 分钟前`
  }

  const hours = Math.floor(minutes / 60)
  if (hours < 24) {
    return variant === 'compact' ? `${hours}h 前` : `${hours} 小时前`
  }

  const days = Math.floor(hours / 24)
  return variant === 'compact' ? `${days}d 前` : `${days} 天前`
}
