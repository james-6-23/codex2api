export type ProxyTestStatus = 'untested' | 'success' | 'error'
export type ProxyStatusBadgeKind = 'error' | 'untested' | 'enabled' | 'disabled'

export interface ProxyTestState {
  enabled: boolean
  test_status: ProxyTestStatus
  test_ip: string
  test_location: string
  test_timezone?: string
  test_latency_ms: number
}

export interface ProxyTestResultState {
  success: boolean
  conclusive?: boolean
  ip?: string
  location?: string
  timezone?: string
  latency_ms?: number
  error?: string
}

export interface ProxyBatchTestEvent {
  type: 'start' | 'progress' | 'complete'
  proxy_id?: number
  current?: number
  total?: number
  success?: number
  failed?: number
  error?: string
  result?: ProxyTestResultState
}

export function chunkProxyTestIDs(
  ids: number[],
  batchSize = 100,
): number[][] {
  if (batchSize <= 0) throw new Error('batchSize must be positive')
  const batches: number[][] = []
  for (let offset = 0; offset < ids.length; offset += batchSize) {
    batches.push(ids.slice(offset, offset + batchSize))
  }
  return batches
}

export function getProxyStatusBadgeKind(
  proxy: Pick<ProxyTestState, 'enabled' | 'test_status'>,
): ProxyStatusBadgeKind {
  if (proxy.test_status === 'error') return 'error'
  if (proxy.test_status === 'untested') return 'untested'
  return proxy.enabled ? 'enabled' : 'disabled'
}

export function applyProxyTestResult<T extends ProxyTestState>(
  proxy: T,
  result: ProxyTestResultState,
): T {
  if (!result.success && result.conclusive === false) {
    return proxy
  }
  if (!result.success) {
    return {
      ...proxy,
      test_status: 'error',
      test_ip: '',
      test_location: '',
      test_latency_ms: 0,
    }
  }
  return {
    ...proxy,
    test_status: 'success',
    test_ip: result.ip || '',
    test_location: result.location || '',
    test_timezone: result.timezone || (proxy.test_ip === result.ip ? proxy.test_timezone || '' : ''),
    test_latency_ms: result.latency_ms || 0,
  }
}

export function parseProxyBatchTestSSELine(
  line: string,
): ProxyBatchTestEvent | null {
  if (!line.startsWith('data:')) return null
  const payload = line.slice(5).trim()
  if (!payload) return null
  try {
    const parsed = JSON.parse(payload) as { type?: unknown }
    if (
      parsed === null ||
      typeof parsed !== 'object' ||
      (parsed.type !== 'start' &&
        parsed.type !== 'progress' &&
        parsed.type !== 'complete')
    ) {
      return null
    }
    return parsed as ProxyBatchTestEvent
  } catch {
    return null
  }
}

export async function readProxyBatchTestSSE(
  response: Response,
  onEvent: (event: ProxyBatchTestEvent) => void,
): Promise<ProxyBatchTestEvent | null> {
  const reader = response.body?.getReader()
  if (!reader) return null

  const decoder = new TextDecoder()
  let buffer = ''
  let completeEvent: ProxyBatchTestEvent | null = null
  const consumeLine = (line: string) => {
    const event = parseProxyBatchTestSSELine(line)
    if (!event) return
    if (event.type === 'complete') completeEvent = event
    onEvent(event)
  }

  for (;;) {
    const { done, value } = await reader.read()
    if (done) break
    buffer += decoder.decode(value, { stream: true })
    const lines = buffer.split('\n')
    buffer = lines.pop() ?? ''
    for (const line of lines) consumeLine(line)
  }
  buffer += decoder.decode()
  if (buffer) consumeLine(buffer)
  return completeEvent
}
