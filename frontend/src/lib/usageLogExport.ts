export async function confirmedUsageLogDownload<Result>(
  confirm: () => Promise<boolean>,
  download: () => Promise<Result>,
  save: (result: Result) => void,
): Promise<boolean> {
  if (!await confirm()) return false
  const blob = await download()
  save(blob)
  return true
}

export function saveUsageLogExport(blob: Blob, scope: 'filtered' | 'all', part?: number) {
  const url = URL.createObjectURL(blob)
  const anchor = document.createElement('a')
  anchor.href = url
  anchor.download = `usage-logs-${scope}-${new Date().toISOString().replace(/[:.]/g, '-')}${part ? `-part-${String(part).padStart(4, '0')}` : ''}.json`
  document.body.appendChild(anchor)
  try {
    anchor.click()
  } finally {
    anchor.remove()
    setTimeout(() => URL.revokeObjectURL(url), 1000)
  }
}

export type UsageLogExportPage = {
  version: number
  metadata: string
  records: string[]
  next_cursor: string
  complete: boolean
}

export type UsageLogExportProgress = { records: number; bytes: number; parts: number }

export async function downloadUsageLogPages(
  fetchPage: (cursor: string, signal: AbortSignal) => Promise<UsageLogExportPage>,
  save: (blob: Blob, part: number) => void,
  signal: AbortSignal,
  onProgress: (progress: UsageLogExportProgress) => void,
  partBytes = 32 * 1024 * 1024,
): Promise<UsageLogExportProgress> {
  const progress: UsageLogExportProgress = { records: 0, bytes: 0, parts: 0 }
  let cursor = ''
  let metadata = ''
  let records: string[] = []
  let bytes = 0
  const encoder = new TextEncoder()
  const flush = (finished: boolean) => {
    signal.throwIfAborted()
    const suffix = `,"total":${records.length},"complete":true,"part":${progress.parts + 1},"export_complete":${finished},"exported_records":${progress.records}}\n`
    const blob = new Blob([metadata.slice(0, -1), ',"logs":[\n', records.join(',\n'), ']', suffix], { type: 'application/json' })
    save(blob, progress.parts + 1)
    progress.parts++
    records = []
    bytes = 0
    onProgress({ ...progress })
  }
  while (true) {
    signal.throwIfAborted()
    let page: UsageLogExportPage | undefined
    for (let attempt = 0; !page; attempt++) {
      try {
        page = await fetchPage(cursor, signal)
      } catch (error) {
        signal.throwIfAborted()
        const status = typeof error === 'object' && error !== null && 'status' in error ? Number(error.status) : 0
        const retryable = error instanceof TypeError || [408, 409, 429, 502, 503, 504].includes(status)
        if (!retryable || attempt >= 2) {
          if (error instanceof TypeError) throw new Error('usage_export_network')
          throw error
        }
        await new Promise<void>((resolve, reject) => {
          const abort = () => { clearTimeout(timer); signal.removeEventListener('abort', abort); reject(signal.reason) }
          const timer = setTimeout(() => { signal.removeEventListener('abort', abort); resolve() }, (attempt + 1) * 500)
          signal.addEventListener('abort', abort, { once: true })
        })
      }
    }
    signal.throwIfAborted()
    if (page.version !== 1 || typeof page.metadata !== 'string' || !page.metadata.endsWith('}') || !Array.isArray(page.records) || !page.records.every(record => typeof record === 'string') || typeof page.complete !== 'boolean' || typeof page.next_cursor !== 'string' || page.complete !== (page.next_cursor === '') || !page.complete && (!page.records.length || page.next_cursor === cursor)) {
      throw new Error('usage_export_invalid_page')
    }
    if (metadata && metadata !== page.metadata) throw new Error('usage_export_changed')
    try {
      const envelope = JSON.parse(page.metadata)
      if (!envelope || Array.isArray(envelope) || typeof envelope !== 'object') throw new Error()
      for (const record of page.records) {
        const entry = JSON.parse(record)
        if (!entry || Array.isArray(entry) || typeof entry !== 'object') throw new Error()
      }
    } catch {
      throw new Error('usage_export_invalid_page')
    }
    metadata = page.metadata
    for (const record of page.records) {
      signal.throwIfAborted()
      const size = encoder.encode(record).byteLength
      if (records.length > 0 && bytes + size > partBytes) flush(false)
      records.push(record)
      bytes += size
      progress.bytes += size
      progress.records++
    }
    onProgress({ ...progress })
    if (page.complete) {
      flush(true)
      return progress
    }
    cursor = page.next_cursor
  }
}
