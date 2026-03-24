export function getErrorMessage(error: unknown, fallback = '未知错误'): string {
  if (error instanceof Error && error.message.trim()) {
    return error.message
  }

  if (typeof error === 'string' && error.trim()) {
    return error
  }

  if (error && typeof error === 'object') {
    if ('error' in error && typeof error.error === 'string' && error.error.trim()) {
      return error.error
    }
    if ('message' in error && typeof error.message === 'string' && error.message.trim()) {
      return error.message
    }
  }

  return fallback
}
