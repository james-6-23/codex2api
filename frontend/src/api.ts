import type {
  AccountEventTrendPoint,
  AccountPortalAuthURLResponse,
  AccountPortalSubmitResponse,
  AccountUsageDetail,
  AddAccountRequest,
  AddATAccountRequest,
  ImportAgentIdentityRequest,
  ImportAgentIdentityResponse,
  AgentIdentityBatchImportRequest,
  AgentIdentityBatchImportResponse,
  AgentIdentityImportItem,
  AddOpenAIResponsesAccountRequest,
  AddGrokAccountRequest,
  UpdateGrokAccountRequest,
  FetchGrokModelsResponse,
  GrokDeviceStartRequest,
  GrokDeviceStartResponse,
  GrokDevicePollRequest,
  GrokDevicePollResponse,
  GrokSSOImportRequest,
  GrokSSOImportResponse,
  GrokBatchImportRequest,
  GrokBatchImportResponse,
  AdminErrorResponse,
  APIKeysResponse,
  APIKeyTokenStat,
  AccountsResponse,
  ChartAggregation,
  CreateAccountResponse,
  CreateAPIKeyResponse,
  CreateAPIKeyRequest,
  FetchOpenAIResponsesModelsRequest,
  FetchOpenAIResponsesModelsResponse,
  CreateImageJobPayload,
  HealthResponse,
  ImageAssetsResponse,
  ImagePromptTemplate,
  ImageJobResponse,
  ImageJobsResponse,
  ImagePromptTemplatePayload,
  ImagePromptTemplatesResponse,
  InviteResponse,
  MessageResponse,
  ModelSyncResponse,
  ModelPricingOverride,
  ModelsResponse,
  OAuthExchangeResponse,
  OAuthURLResponse,
  OpsErrorSummary,
  OpsOverviewResponse,
  PromptFilterLog,
  PromptFilterLogsResponse,
  PromptFilterRulePatternTestResponse,
  PromptFilterRulesResponse,
  PromptFilterTestResponse,
  PublicAPIKeyUsageResponse,
  RecycleBinAccountsResponse,
  ResetCreditsDetailResponse,
  RuntimeStatusResponse,
  SiteBranding,
  StatsResponse,
  SystemUpdateInfo,
  SystemUpdateResult,
  SetupHintsResponse,
  CPAExportEntry,
  SystemSettings,
  ObservedInstructionsResponse,
  UpdateAccountSchedulerRequest,
  UpdateAPIKeyRequest,
  UpdateOAuthAccountRequest,
  UpdateOpenAIResponsesAccountRequest,
  UsageLogsResponse,
  UsageLogsPagedResponse,
  UsageStats,
  AccountGroup,
  AccountGroupsResponse,
  AccountHealthBarsResponse,
  BatchUpdateAccountsRequest,
  BackgroundUploadResponse,
  CreateAccountGroupRequest,
  UpdateAccountGroupRequest,
} from './types'

const BASE = '/api/admin'
export const ADMIN_AUTH_REQUIRED_EVENT = 'codex2api:admin-auth-required'
const ADMIN_AUTH_RESET_KEY = 'admin_auth_reset_at'

export function getAdminKey(): string {
  return localStorage.getItem('admin_key') ?? ''
}

export function clearAdminKey() {
  localStorage.removeItem('admin_key')
}

export function setAdminKey(key: string) {
  if (key) {
    localStorage.setItem('admin_key', key)
  } else {
    clearAdminKey()
  }
}

export function resetAdminAuthState() {
  clearAdminKey()
  localStorage.setItem(ADMIN_AUTH_RESET_KEY, String(Date.now()))
  window.dispatchEvent(new Event(ADMIN_AUTH_REQUIRED_EVENT))
}

// RequestInit 扩展:timeoutMs 可选,开启后到时自动 abort 请求。
type RequestOptions = RequestInit & { timeoutMs?: number }

function extractAdminErrorMessage(body: string, status: number): string {
  if (!body.trim()) {
    return `HTTP ${status}`
  }

  try {
    const parsed = JSON.parse(body) as Partial<AdminErrorResponse>
    if (typeof parsed.error === 'string' && parsed.error.trim()) {
      return parsed.error
    }
  } catch {
    // ignore JSON parse error and fall back to raw text
  }

  return body
}

async function request<T>(path: string, options: RequestOptions = {}): Promise<T> {
  const headers = new Headers(options.headers)
  const isFormData = typeof FormData !== 'undefined' && options.body instanceof FormData
  if (options.body !== undefined && options.body !== null && !isFormData && !headers.has('Content-Type')) {
    headers.set('Content-Type', 'application/json')
  }

  const adminKey = getAdminKey()
  if (adminKey) {
    headers.set('X-Admin-Key', adminKey)
  }

  // 可选的客户端超时:调用方通过 timeoutMs 显式开启,不传则保持原有“无超时”行为,
  // 避免影响下载/导出等长耗时接口。仅在调用方未自带 signal 时接管中止逻辑。
  const { timeoutMs, ...init } = options
  let timeoutId: ReturnType<typeof setTimeout> | undefined
  if (timeoutMs && timeoutMs > 0 && !init.signal && typeof AbortController !== 'undefined') {
    const controller = new AbortController()
    init.signal = controller.signal
    timeoutId = setTimeout(() => controller.abort(), timeoutMs)
  }

  let res: Response
  try {
    res = await fetch(BASE + path, {
      ...init,
      cache: init.cache ?? 'no-store',
      headers,
    })
  } catch (err) {
    if (timeoutId !== undefined && err instanceof DOMException && err.name === 'AbortError') {
      throw new Error('请求超时，请稍后重试')
    }
    throw err
  } finally {
    if (timeoutId !== undefined) clearTimeout(timeoutId)
  }

  if (!res.ok) {
    const body = await res.text()
    if (res.status === 401) {
      resetAdminAuthState()
    }
    throw new Error(extractAdminErrorMessage(body, res.status))
  }

  return (await res.json()) as T
}

async function requestPublic<T>(path: string, options: RequestInit = {}): Promise<T> {
  const res = await fetch(path, {
    ...options,
    cache: options.cache ?? 'no-store',
  })

  if (!res.ok) {
    const body = await res.text()
    throw new Error(extractAdminErrorMessage(body, res.status))
  }

  return (await res.json()) as T
}

// 公开账号自助门户:无鉴权头,错误体形如 {error:{message}}。
// 单独实现以便解析嵌套的 message 并携带 HTTP 状态码(供 404「门户未开启」判定)。
async function requestAccountPortal<T>(path: string, options: RequestInit = {}): Promise<T> {
  const headers = new Headers(options.headers)
  if (options.body !== undefined && options.body !== null && !headers.has('Content-Type')) {
    headers.set('Content-Type', 'application/json')
  }

  const res = await fetch(path, {
    ...options,
    cache: options.cache ?? 'no-store',
    headers,
  })

  if (!res.ok) {
    const body = await res.text()
    let message = body.trim() || `HTTP ${res.status}`
    try {
      const parsed = JSON.parse(body) as { error?: { message?: string } | string; message?: string }
      if (parsed && typeof parsed.error === 'object' && parsed.error?.message) {
        message = parsed.error.message
      } else if (typeof parsed.error === 'string' && parsed.error.trim()) {
        message = parsed.error
      } else if (typeof parsed.message === 'string' && parsed.message.trim()) {
        message = parsed.message
      }
    } catch {
      // 保留原始文本
    }
    const err = new Error(message) as Error & { status?: number }
    err.status = res.status
    throw err
  }

  const text = await res.text()
  return (text ? JSON.parse(text) : undefined) as T
}

async function requestAPIKeyUsage<T>(path: string, apiKey: string, options: RequestInit = {}): Promise<T> {
  const headers = new Headers(options.headers)
  headers.set('Authorization', `Bearer ${apiKey}`)

  const res = await fetch('/api/key-usage' + path, {
    ...options,
    cache: options.cache ?? 'no-store',
    headers,
  })

  if (!res.ok) {
    const body = await res.text()
    throw new Error(extractAdminErrorMessage(body, res.status))
  }

  return (await res.json()) as T
}

async function requestImageStudioPortal<T>(path: string, apiKey: string, options: RequestInit = {}): Promise<T> {
  const headers = new Headers(options.headers)
  headers.set('Authorization', `Bearer ${apiKey}`)
  if (options.body && !(options.body instanceof FormData) && !headers.has('Content-Type')) {
    headers.set('Content-Type', 'application/json')
  }

  const res = await fetch('/api/image-studio' + path, {
    ...options,
    cache: options.cache ?? 'no-store',
    headers,
  })

  if (!res.ok) {
    const body = await res.text()
    throw new Error(extractAdminErrorMessage(body, res.status))
  }

  if (res.status === 204) {
    return undefined as T
  }
  const text = await res.text()
  if (!text) {
    return undefined as T
  }
  return JSON.parse(text) as T
}

async function requestImageStudioPortalBlob(path: string, apiKey: string, options: RequestInit = {}): Promise<Blob> {
  const headers = new Headers(options.headers)
  headers.set('Authorization', `Bearer ${apiKey}`)

  const res = await fetch('/api/image-studio' + path, {
    ...options,
    cache: options.cache ?? 'no-store',
    headers,
  })

  if (!res.ok) {
    const body = await res.text()
    throw new Error(extractAdminErrorMessage(body, res.status))
  }

  return res.blob()
}

async function requestBlob(path: string, options: RequestInit = {}): Promise<Blob> {
  const headers = new Headers(options.headers)

  const adminKey = getAdminKey()
  if (adminKey) {
    headers.set('X-Admin-Key', adminKey)
  }

  const res = await fetch(BASE + path, {
    ...options,
    cache: options.cache ?? 'no-store',
    headers,
  })

  if (!res.ok) {
    const body = await res.text()
    if (res.status === 401) {
      resetAdminAuthState()
    }
    throw new Error(extractAdminErrorMessage(body, res.status))
  }

  return res.blob()
}

function buildOpsErrorSearchParams(params: {
  start: string
  end: string
  status?: string
  errorKind?: string
  endpoint?: string
  apiKeyId?: string
  stream?: string
  fast?: string
  q?: string
  dedupe?: boolean
  excludeStatus?: string
}) {
  const search = new URLSearchParams()
  search.set('start', params.start)
  search.set('end', params.end)
  if (params.status) search.set('status', params.status)
  if (params.errorKind) search.set('error_kind', params.errorKind)
  if (params.endpoint) search.set('endpoint', params.endpoint)
  if (params.apiKeyId) search.set('api_key_id', params.apiKeyId)
  if (params.stream) search.set('stream', params.stream)
  if (params.fast) search.set('fast', params.fast)
  if (params.q) search.set('q', params.q)
  if (typeof params.dedupe === 'boolean') search.set('dedupe', String(params.dedupe))
  if (params.excludeStatus) search.set('exclude_status', params.excludeStatus)
  return search
}

export const api = {
  getBranding: () => requestPublic<SiteBranding>('/api/branding'),
  // 公开账号自助门户:生成 OpenAI 授权链接(无鉴权)。
  generateAccountPortalAuthURL: (data: { contact_email: string }) =>
    requestAccountPortal<AccountPortalAuthURLResponse>('/api/account-portal/generate-auth-url', {
      method: 'POST',
      body: JSON.stringify(data),
    }),
  // 公开账号自助门户:提交授权码(无鉴权)。
  submitAccountPortalCode: (data: { session_id: string; code: string; state: string }) =>
    requestAccountPortal<AccountPortalSubmitResponse>('/api/account-portal/submit-code', {
      method: 'POST',
      body: JSON.stringify(data),
    }),
  getPublicAPIKeyUsage: (apiKey: string, range = '30d', params: { page?: number; pageSize?: number } = {}) => {
    const search = new URLSearchParams()
    search.set('range', range)
    if (params.page) search.set('page', String(params.page))
    if (params.pageSize) search.set('page_size', String(params.pageSize))
    return requestAPIKeyUsage<PublicAPIKeyUsageResponse>(`/summary?${search.toString()}`, apiKey)
  },
  createPortalImageJob: (apiKey: string, data: CreateImageJobPayload) =>
    requestImageStudioPortal<ImageJobResponse>('/jobs', apiKey, { method: 'POST', body: JSON.stringify(data) }),
  createPortalImageEditJob: (apiKey: string, data: CreateImageJobPayload) =>
    requestImageStudioPortal<ImageJobResponse>('/edit-jobs', apiKey, { method: 'POST', body: JSON.stringify(data) }),
  getPortalImageJobs: (apiKey: string, params: { page?: number; pageSize?: number } = {}) => {
    const sp = new URLSearchParams()
    if (params.page) sp.set('page', String(params.page))
    if (params.pageSize) sp.set('page_size', String(params.pageSize))
    return requestImageStudioPortal<ImageJobsResponse>(`/jobs?${sp.toString()}`, apiKey)
  },
  getPortalImageJob: (apiKey: string, id: number, params: { includeCache?: boolean } = {}) => {
    const sp = new URLSearchParams()
    if (params.includeCache) sp.set('include_cache', '1')
    const query = sp.toString()
    return requestImageStudioPortal<ImageJobResponse>(`/jobs/${id}${query ? `?${query}` : ''}`, apiKey)
  },
  deletePortalImageJob: (apiKey: string, id: number) =>
    requestImageStudioPortal<MessageResponse>(`/jobs/${id}`, apiKey, { method: 'DELETE' }),
  getPortalImageAssets: (apiKey: string, params: { page?: number; pageSize?: number } = {}) => {
    const sp = new URLSearchParams()
    if (params.page) sp.set('page', String(params.page))
    if (params.pageSize) sp.set('page_size', String(params.pageSize))
    return requestImageStudioPortal<ImageAssetsResponse>(`/assets?${sp.toString()}`, apiKey)
  },
  getPortalImageAssetFile: (apiKey: string, id: number, download = false, thumbKB = 0) => {
    const sp = new URLSearchParams()
    if (download) sp.set('download', '1')
    if (thumbKB > 0) sp.set('thumb_kb', String(thumbKB))
    const query = sp.toString()
    return requestImageStudioPortalBlob(`/assets/${id}/file${query ? `?${query}` : ''}`, apiKey)
  },
  deletePortalImageAsset: (apiKey: string, id: number) =>
    requestImageStudioPortal<MessageResponse>(`/assets/${id}`, apiKey, { method: 'DELETE' }),
  getStats: () => request<StatsResponse>('/stats'),
  getAccounts: () => request<AccountsResponse>('/accounts'),
  addAccount: (data: AddAccountRequest) =>
    request<CreateAccountResponse>('/accounts', { method: 'POST', body: JSON.stringify(data) }),
  addATAccount: (data: AddATAccountRequest) =>
    request<CreateAccountResponse>('/accounts/at', { method: 'POST', body: JSON.stringify(data) }),
  importCodexAgentIdentity: (data: ImportAgentIdentityRequest) =>
    request<ImportAgentIdentityResponse>('/accounts/codex/agent-identity', { method: 'POST', body: JSON.stringify(data) }),
  batchImportCodexAgentIdentity: (data: AgentIdentityBatchImportRequest) =>
    request<AgentIdentityBatchImportResponse>('/accounts/codex/agent-identity/import', { method: 'POST', body: JSON.stringify(data) }),
  addOpenAIResponsesAccount: (data: AddOpenAIResponsesAccountRequest) =>
    request<CreateAccountResponse>('/accounts/openai-responses', { method: 'POST', body: JSON.stringify(data) }),
  fetchOpenAIResponsesModels: (data: FetchOpenAIResponsesModelsRequest) =>
    request<FetchOpenAIResponsesModelsResponse>('/accounts/openai-responses/models', { method: 'POST', body: JSON.stringify(data) }),
  updateOpenAIResponsesAccount: (id: number, data: UpdateOpenAIResponsesAccountRequest) =>
    request<MessageResponse>(`/accounts/${id}/openai-responses`, { method: 'PATCH', body: JSON.stringify(data) }),
  addGrokAccount: (data: AddGrokAccountRequest) =>
    request<CreateAccountResponse>('/accounts/grok', { method: 'POST', body: JSON.stringify(data) }),
  fetchGrokModels: (data: AddGrokAccountRequest) =>
    request<FetchGrokModelsResponse>('/accounts/grok/models', { method: 'POST', body: JSON.stringify(data) }),
  startGrokDeviceAuth: (data: GrokDeviceStartRequest = {}) =>
    request<GrokDeviceStartResponse>('/accounts/grok/oauth/device/start', {
      method: 'POST',
      body: JSON.stringify(data),
    }),
  pollGrokDeviceAuth: (data: GrokDevicePollRequest) =>
    request<GrokDevicePollResponse>('/accounts/grok/oauth/device/poll', {
      method: 'POST',
      body: JSON.stringify(data),
    }),
  importGrokSSO: (data: GrokSSOImportRequest) =>
    request<GrokSSOImportResponse>('/accounts/grok/sso/import', {
      method: 'POST',
      body: JSON.stringify(data),
    }),
  batchImportGrokAccounts: (data: GrokBatchImportRequest) =>
    request<GrokBatchImportResponse>('/accounts/grok/import', {
      method: 'POST',
      body: JSON.stringify(data),
    }),
  importGrokRefreshTokens: (data: GrokSSOImportRequest) =>
    request<GrokSSOImportResponse>('/accounts/grok/refresh/import', {
      method: 'POST',
      body: JSON.stringify(data),
    }),
  updateGrokAccount: (id: number, data: UpdateGrokAccountRequest) =>
    request<MessageResponse>(`/accounts/${id}/grok`, { method: 'PATCH', body: JSON.stringify(data) }),
  deleteAccount: (id: number) =>
    request<MessageResponse>(`/accounts/${id}`, { method: 'DELETE' }),
  updateAccountNote: (id: number, note: string) =>
    request<MessageResponse>(`/accounts/${id}/note`, { method: 'PATCH', body: JSON.stringify({ note }) }),
  getRecycleBinAccounts: () =>
    request<RecycleBinAccountsResponse>('/accounts/recycle-bin'),
  restoreAccount: (id: number) =>
    request<MessageResponse>(`/accounts/${id}/restore`, { method: 'POST' }),
  purgeAccount: (id: number) =>
    request<MessageResponse>(`/accounts/${id}/purge`, { method: 'DELETE' }),
  emptyRecycleBin: () =>
    request<{ message: string; purged: number }>('/accounts/recycle-bin', {
      method: 'DELETE',
      body: JSON.stringify({ confirm: 'EMPTY-RECYCLE-BIN' }),
    }),
  refreshAccount: (id: number) =>
    request<MessageResponse>(`/accounts/${id}/refresh`, { method: 'POST' }),
  forceUsageProbe: () =>
    request<{ triggered: boolean; concurrency: number; reason?: string; mode?: string }>(`/accounts/usage/probe`, { method: 'POST' }),
  refreshAccountUsage: (id: number) =>
    request<{
      refreshed: boolean
      usage_percent_5h?: number
      usage_percent_7d?: number
      reset_5h_at?: string
      reset_7d_at?: string
    }>(`/accounts/${id}/usage/refresh`, { method: 'POST' }),
  updateAccountScheduler: (id: number, data: UpdateAccountSchedulerRequest) =>
    request<MessageResponse>(`/accounts/${id}/scheduler`, { method: 'PATCH', body: JSON.stringify(data) }),
  // 设置 OAuth 账号的支持模型白名单;空数组表示清空(该账号可调度所有模型)。返回归一化后的白名单。
  updateAccountModels: (id: number, models: string[]) =>
    request<{ models: string[] }>(`/accounts/${id}/models`, { method: 'PATCH', body: JSON.stringify({ models }) }),
  // 拉取该账号真实的上游模型清单(slug 列表,不落库),供白名单编辑器合并使用。
  syncAccountModelsUpstream: (id: number) =>
    request<{ models: string[] }>(`/accounts/${id}/models/sync-upstream`, { method: 'POST' }),
  // 用账号自身凭据并发探测系统文本模型(已排除 image),返回确认可用的模型及每个模型的判定明细。只读不落库。
  probeAccountModels: (id: number) =>
    request<{
      available: string[];
      results: { model: string; outcome: string; detail?: string }[];
    }>(`/accounts/${id}/models/probe`, { method: 'POST' }),
  listAccountGroups: () => request<AccountGroupsResponse>('/account-groups'),
  createAccountGroup: (data: CreateAccountGroupRequest) =>
    request<{ id: number; message: string }>('/account-groups', { method: 'POST', body: JSON.stringify(data) }),
  updateAccountGroup: (id: number, data: UpdateAccountGroupRequest) =>
    request<MessageResponse>(`/account-groups/${id}`, { method: 'PATCH', body: JSON.stringify(data) }),
  deleteAccountGroup: (id: number, force = false) =>
    request<MessageResponse>(`/account-groups/${id}${force ? '?force=true' : ''}`, { method: 'DELETE' }),
  toggleAccountEnabled: (id: number, enabled: boolean) =>
    request<MessageResponse>(`/accounts/${id}/enable`, { method: 'POST', body: JSON.stringify({ enabled }) }),
  toggleAccountLock: (id: number, locked: boolean) =>
    request<MessageResponse>(`/accounts/${id}/lock`, { method: 'POST', body: JSON.stringify({ locked }) }),
  batchUpdateAccounts: (data: BatchUpdateAccountsRequest) =>
    request<{ message: string; success: number; failed: number }>('/accounts/batch-update', { method: 'POST', body: JSON.stringify(data) }),
  resetAccountStatus: (id: number) =>
    request<MessageResponse>(`/accounts/${id}/reset-status`, { method: 'POST' }),
  resetCredits: (id: number) =>
    request<{ message: string; rate_limit_reset_credits?: number }>(`/accounts/${id}/reset-credits`, { method: 'POST' }),
  getResetCredits: (id: number) =>
    request<ResetCreditsDetailResponse>(`/accounts/${id}/reset-credits`),
  getAccountHealthBars: () =>
    request<AccountHealthBarsResponse>('/accounts/health-bars'),
  sendInvite: (id: number, data: { emails?: string[]; emails_text?: string; referral_key?: string; proxy_url?: string; max_emails?: number }) =>
    request<InviteResponse>(`/accounts/${id}/invite`, { method: 'POST', body: JSON.stringify(data) }),
  batchResetStatus: (ids: number[]) =>
    request<{ message: string; success: number; failed: number }>('/accounts/batch-reset-status', { method: 'POST', body: JSON.stringify({ ids }) }),
  batchDeleteAccounts: (ids: number[]) =>
    request<{ message: string; deleted: number; success: number; failed: number }>('/accounts/batch-delete', { method: 'POST', body: JSON.stringify({ ids }) }),
  batchRefreshAccounts: (ids: number[]) =>
    request<{ message: string; success: number; failed: number }>('/accounts/batch-refresh', { method: 'POST', body: JSON.stringify({ ids }) }),
  getAccountUsage: (id: number, days?: number) => {
    const search = new URLSearchParams()
    if (typeof days === 'number') search.set('days', String(days))
    const qs = search.toString()
    return request<AccountUsageDetail>(`/accounts/${id}/usage${qs ? `?${qs}` : ''}`)
  },
  updateAccountCredit: (id: number, data: { credit_enabled: boolean; credit_skip_usage_window: boolean }) =>
    request<MessageResponse>(`/accounts/${id}/credit`, { method: 'PATCH', body: JSON.stringify(data) }),
  getHealth: () => request<HealthResponse>('/health'),
  getPromptFilterNewAPISecret: () => request<{ configured: boolean; source: 'none' | 'database' | 'environment'; masked: string; secret?: string }>('/prompt-filter/newapi-secret'),
  generatePromptFilterNewAPISecret: () => request<{ configured: boolean; source: string; masked: string; secret: string }>('/prompt-filter/newapi-secret/generate', { method: 'POST' }),
  replacePromptFilterNewAPISecret: (secret: string) => request<{ configured: boolean; source: string; masked: string; secret: string }>('/prompt-filter/newapi-secret', { method: 'PUT', body: JSON.stringify({ secret }) }),
  getOpsOverview: () => request<OpsOverviewResponse>('/ops/overview'),
  getRuntimeStatus: () => request<RuntimeStatusResponse>('/runtime-status'),
  getSystemUpdate: () => request<SystemUpdateInfo>('/system/update', { timeoutMs: 20_000 }),
  performSystemUpdate: () =>
    // 后端下载上游二进制最长约 10 分钟,客户端给到 11 分钟兜底:既不会误伤慢下载,
    // 又能保证请求最终有界返回,不会无限期卡在“更新中”。
    request<SystemUpdateResult>('/system/update', { method: 'POST', timeoutMs: 11 * 60_000 }),
  getOpsErrorSummary: (params: {
    start: string
    end: string
    status?: string
    errorKind?: string
    endpoint?: string
    apiKeyId?: string
    stream?: string
    fast?: string
    q?: string
  }) => {
    const search = buildOpsErrorSearchParams(params)
    return request<OpsErrorSummary>(`/ops/errors/summary?${search.toString()}`)
  },
  getOpsErrors: (params: {
    start: string
    end: string
    page: number
    pageSize?: number
    status?: string
    errorKind?: string
    endpoint?: string
    apiKeyId?: string
    stream?: string
    fast?: string
    q?: string
  }) => {
    const search = buildOpsErrorSearchParams(params)
    search.set('page', String(params.page))
    if (params.pageSize) search.set('page_size', String(params.pageSize))
    return request<UsageLogsPagedResponse>(`/ops/errors?${search.toString()}`)
  },
  downloadOpsErrors: (params: {
    start: string
    end: string
    status?: string
    errorKind?: string
    endpoint?: string
    apiKeyId?: string
    stream?: string
    fast?: string
    q?: string
    dedupe?: boolean
    excludeStatus?: string
  }) => {
    const search = buildOpsErrorSearchParams(params)
    return requestBlob(`/ops/errors/export?${search.toString()}`)
  },
  getUsageStats: (params: { start?: string; end?: string; channel?: string } = {}) => {
    const searchParams = new URLSearchParams()
    if (params.start) searchParams.set('start', params.start)
    if (params.end) searchParams.set('end', params.end)
    if (params.channel) searchParams.set('channel', params.channel)
    const qs = searchParams.toString()
    return request<UsageStats>(qs ? `/usage/stats?${qs}` : '/usage/stats')
  },
  getAPIKeyTokenStats: (params: { start?: string; end?: string } = {}) => {
    const searchParams = new URLSearchParams()
    if (params.start) searchParams.set('start', params.start)
    if (params.end) searchParams.set('end', params.end)
    const qs = searchParams.toString()
    return request<{ items: APIKeyTokenStat[] }>(
      qs ? `/usage/api-keys?${qs}` : '/usage/api-keys',
    )
  },
  getUsageLogs: (params: { start?: string; end?: string; limit?: number } = {}) => {
    const searchParams = new URLSearchParams()
    if (params.start && params.end) {
      searchParams.set('start', params.start)
      searchParams.set('end', params.end)
    } else if (params.limit) {
      searchParams.set('limit', String(params.limit))
    }
    return request<UsageLogsResponse>(`/usage/logs?${searchParams.toString()}`)
  },
  getUsageLogsPaged: (params: { start: string; end: string; page: number; pageSize?: number; email?: string; model?: string; endpoint?: string; apiKeyId?: string; accountId?: string; fast?: string; stream?: string; channel?: string }) => {
    const searchParams = new URLSearchParams()
    searchParams.set('start', params.start)
    searchParams.set('end', params.end)
    searchParams.set('page', String(params.page))
    if (params.pageSize) searchParams.set('page_size', String(params.pageSize))
    if (params.email) searchParams.set('email', params.email)
    if (params.model) searchParams.set('model', params.model)
    if (params.endpoint) searchParams.set('endpoint', params.endpoint)
    if (params.apiKeyId) searchParams.set('api_key_id', params.apiKeyId)
    if (params.accountId) searchParams.set('account_id', params.accountId)
    if (params.fast) searchParams.set('fast', params.fast)
    if (params.stream) searchParams.set('stream', params.stream)
    if (params.channel) searchParams.set('channel', params.channel)
    return request<UsageLogsPagedResponse>(`/usage/logs?${searchParams.toString()}`)
  },
  getChartData: (params: { start: string; end: string; bucketMinutes: number; channel?: string }) => {
    const searchParams = new URLSearchParams()
    searchParams.set('start', params.start)
    searchParams.set('end', params.end)
    searchParams.set('bucket_minutes', String(params.bucketMinutes))
    if (params.channel) searchParams.set('channel', params.channel)
    return request<ChartAggregation>(`/usage/chart-data?${searchParams.toString()}`)
  },
  getAccountEventTrend: (params: { start: string; end: string; bucketMinutes: number }) => {
    const sp = new URLSearchParams()
    sp.set('start', params.start)
    sp.set('end', params.end)
    sp.set('bucket_minutes', String(params.bucketMinutes))
    return request<{ trend: AccountEventTrendPoint[] }>(`/accounts/event-trend?${sp.toString()}`)
  },
  getAPIKeys: () => request<APIKeysResponse>('/keys'),
  createAPIKey: (data: CreateAPIKeyRequest) =>
    request<CreateAPIKeyResponse>('/keys', {
      method: 'POST',
      body: JSON.stringify(data),
    }),
  deleteAPIKey: (id: number) =>
    request<MessageResponse>(`/keys/${id}`, { method: 'DELETE' }),
  updateAPIKey: (id: number, data: UpdateAPIKeyRequest) =>
    request<MessageResponse>(`/keys/${id}`, { method: 'PATCH', body: JSON.stringify(data) }),
  getImagePromptTemplates: (params: { q?: string; tag?: string } = {}) => {
    const sp = new URLSearchParams()
    if (params.q) sp.set('q', params.q)
    if (params.tag) sp.set('tag', params.tag)
    const query = sp.toString()
    return request<ImagePromptTemplatesResponse>(`/image-prompts${query ? `?${query}` : ''}`)
  },
  createImagePromptTemplate: (data: ImagePromptTemplatePayload) =>
    request<{ template: ImagePromptTemplate }>('/image-prompts', { method: 'POST', body: JSON.stringify(data) }),
  updateImagePromptTemplate: (id: number, data: ImagePromptTemplatePayload) =>
    request<{ template: ImagePromptTemplate }>(`/image-prompts/${id}`, { method: 'PATCH', body: JSON.stringify(data) }),
  deleteImagePromptTemplate: (id: number) =>
    request<MessageResponse>(`/image-prompts/${id}`, { method: 'DELETE' }),
  createImageJob: (data: CreateImageJobPayload) =>
    request<ImageJobResponse>('/images/jobs', { method: 'POST', body: JSON.stringify(data) }),
  createImageEditJob: (data: CreateImageJobPayload) =>
    request<ImageJobResponse>('/images/edit-jobs', { method: 'POST', body: JSON.stringify(data) }),
  getImageJobs: (params: { page?: number; pageSize?: number } = {}) => {
    const sp = new URLSearchParams()
    if (params.page) sp.set('page', String(params.page))
    if (params.pageSize) sp.set('page_size', String(params.pageSize))
    return request<ImageJobsResponse>(`/images/jobs?${sp.toString()}`)
  },
  getImageJob: (id: number, params: { includeCache?: boolean } = {}) => {
    const sp = new URLSearchParams()
    if (params.includeCache) sp.set('include_cache', '1')
    const query = sp.toString()
    return request<ImageJobResponse>(`/images/jobs/${id}${query ? `?${query}` : ''}`)
  },
  deleteImageJob: (id: number) =>
    request<MessageResponse>(`/images/jobs/${id}`, { method: 'DELETE' }),
  getImageAssets: (params: { page?: number; pageSize?: number } = {}) => {
    const sp = new URLSearchParams()
    if (params.page) sp.set('page', String(params.page))
    if (params.pageSize) sp.set('page_size', String(params.pageSize))
    return request<ImageAssetsResponse>(`/images/assets?${sp.toString()}`)
  },
  getImageAssetFile: (id: number, download = false, thumbKB = 0) => {
    const sp = new URLSearchParams()
    if (download) sp.set('download', '1')
    if (thumbKB > 0) sp.set('thumb_kb', String(thumbKB))
    const query = sp.toString()
    return requestBlob(`/images/assets/${id}/file${query ? `?${query}` : ''}`)
  },
  deleteImageAsset: (id: number) =>
    request<MessageResponse>(`/images/assets/${id}`, { method: 'DELETE' }),
  clearUsageLogs: () =>
    request<MessageResponse>('/usage/logs', { method: 'DELETE' }),
  getSetupHints: () => request<SetupHintsResponse>('/setup-hints'),
  getSettings: () => request<SystemSettings>('/settings'),
  getObservedInstructions: () =>
    request<ObservedInstructionsResponse>('/settings/observed-instructions'),
  updateSettings: (data: Partial<SystemSettings>) =>
    request<SystemSettings>('/settings', { method: 'PUT', body: JSON.stringify(data) }),
  uploadBackground: (file: File) => {
    const form = new FormData()
    form.set('file', file)
    return request<BackgroundUploadResponse>('/settings/background-upload', { method: 'POST', body: form })
  },
  testImageStorageConnection: (data: {
    endpoint: string
    region: string
    bucket: string
    access_key: string
    secret_key: string
    prefix: string
    force_path_style: boolean
  }) =>
    request<{ ok: boolean; bucket: string }>('/settings/image-storage/test', {
      method: 'POST',
      body: JSON.stringify(data),
    }),
  getPromptFilterLogs: (params: number | { page?: number; pageSize?: number; limit?: number; source?: string; action?: string; endpoint?: string; model?: string; apiKeyId?: string; q?: string } = 100) => {
    const search = new URLSearchParams()
    if (typeof params === 'number') {
      search.set('limit', String(params))
    } else {
      if (params.page) search.set('page', String(params.page))
      if (params.pageSize) search.set('page_size', String(params.pageSize))
      if (params.limit) search.set('limit', String(params.limit))
      if (params.source) search.set('source', params.source)
      if (params.action) search.set('action', params.action)
      if (params.endpoint) search.set('endpoint', params.endpoint)
      if (params.model) search.set('model', params.model)
      if (params.apiKeyId) search.set('api_key_id', params.apiKeyId)
      if (params.q) search.set('q', params.q)
    }
    return request<PromptFilterLogsResponse>(`/prompt-filter/logs?${search.toString()}`)
  },
  clearPromptFilterLogs: () =>
    request<MessageResponse>('/prompt-filter/logs', { method: 'DELETE' }),
  matchPromptFilterLog: (params: { at: string; endpoint?: string; apiKeyId?: number; source?: string }) => {
    const search = new URLSearchParams()
    search.set('at', params.at)
    if (params.endpoint) search.set('endpoint', params.endpoint)
    if (params.apiKeyId) search.set('api_key_id', String(params.apiKeyId))
    if (params.source) search.set('source', params.source)
    return request<{ found: boolean; log: PromptFilterLog | null }>(`/prompt-filter/logs/match?${search.toString()}`)
  },
  testPromptFilter: (data: { text: string; endpoint?: string; model?: string }) =>
    request<PromptFilterTestResponse>('/prompt-filter/test', { method: 'POST', body: JSON.stringify(data) }),
  testPromptFilterRulePattern: (data: { pattern: string; text: string }) =>
    request<PromptFilterRulePatternTestResponse>('/prompt-filter/rules/test', { method: 'POST', body: JSON.stringify(data) }),
  getPromptFilterRules: () =>
    request<PromptFilterRulesResponse>('/prompt-filter/rules'),
  runPromptIntelligence: () =>
    request<import('./types').PromptIntelligenceRun>('/prompt-filter/intelligence/run', { method: 'POST' }),
  getPromptIntelligenceHistory: (page = 1, pageSize = 20) =>
    request<import('./types').PromptIntelligenceHistoryResponse>(`/prompt-filter/intelligence/history?page=${page}&page_size=${pageSize}`),
  addPromptIntelligenceRule: (candidate: import('./types').PromptIntelligenceCandidate) =>
    request<{ added: number; updated: number }>('/prompt-filter/intelligence/rules', { method: 'POST', body: JSON.stringify(candidate) }),
  getModels: () => request<ModelsResponse>('/models'),
  syncModels: () => request<ModelSyncResponse>('/models/sync', { method: 'POST' }),
  syncCodexCLIVersion: () =>
    request<{
      fetched_version: string
      effective_version: string
      builtin_version: string
      updated: boolean
    }>('/codex-cli-version/sync', { method: 'POST' }),
  listModelPricing: () =>
    request<{
      models: Array<{ model: string; source: string; pricing: ModelPricingOverride }>
      sync_url: string
      default_sync_url: string
      models_dev_url: string
    }>('/model-pricing'),
  updateModelPricing: (payload: { model: string; reset?: boolean; pricing?: ModelPricingOverride }) =>
    request<{ model: string; reset: boolean }>('/model-pricing', {
      method: 'PUT',
      body: JSON.stringify(payload),
    }),
  syncModelPricing: (url: string) =>
    request<{ source_url: string; fetched: number; applied: number; skipped: number }>('/model-pricing/sync', {
      method: 'POST',
      body: JSON.stringify({ url: url ?? '' }),
    }),
  batchTestAccounts: (ids?: number[]) =>
    request<{ total: number; success: number; failed: number; banned: number; rate_limited: number }>('/accounts/batch-test', {
      method: 'POST',
      body: ids ? JSON.stringify({ ids }) : undefined,
    }),
  cleanBanned: () =>
    request<{ message: string; cleaned: number }>('/accounts/clean-banned', { method: 'POST' }),
  cleanRateLimited: () =>
    request<{ message: string; cleaned: number }>('/accounts/clean-rate-limited', { method: 'POST' }),
  cleanError: () =>
    request<{ message: string; cleaned: number }>('/accounts/clean-error', { method: 'POST' }),
  exportAccounts: (params: { filter: 'healthy' | 'all'; ids?: number[] }) => {
    const sp = new URLSearchParams({ filter: params.filter })
    if (params.ids && params.ids.length > 0) sp.set('ids', params.ids.join(','))
    return request<CPAExportEntry[]>(`/accounts/export?${sp.toString()}`)
  },
  /** 导出回收站账号；ids 为空则导出回收站全部。 */
  exportRecycleBinAccounts: (ids?: number[]) => {
    const sp = new URLSearchParams()
    if (ids && ids.length > 0) sp.set('ids', ids.join(','))
    const q = sp.toString()
    return request<CPAExportEntry[]>(`/accounts/recycle-bin/export${q ? `?${q}` : ''}`)
  },
  downloadAccountAuthJSON: (id: number) =>
    requestBlob(`/accounts/${id}/auth-json`),
  migrateAccounts: (data: { url: string; admin_key: string }) =>
    request<{ message: string; total: number; imported: number; duplicate: number; failed: number }>(
      '/accounts/migrate', { method: 'POST', body: JSON.stringify(data) }),
  // Proxies
  listProxies: () =>
    request<{ proxies: ProxyRow[] }>('/proxies'),
  addProxies: (data: { urls?: string[]; url?: string; label?: string }) =>
    request<{ message: string; inserted: number; total: number }>('/proxies', { method: 'POST', body: JSON.stringify(data) }),
  deleteProxy: (id: number) =>
    request<MessageResponse>(`/proxies/${id}`, { method: 'DELETE' }),
  updateProxy: (id: number, data: { url?: string; label?: string; enabled?: boolean }) =>
    request<MessageResponse>(`/proxies/${id}`, { method: 'PATCH', body: JSON.stringify(data) }),
  batchDeleteProxies: (ids: number[]) =>
    request<{ message: string; deleted: number }>('/proxies/batch-delete', { method: 'POST', body: JSON.stringify({ ids }) }),
  testProxy: (url: string, id?: number, lang?: string) =>
    request<ProxyTestResult>('/proxies/test', { method: 'POST', body: JSON.stringify({ url, id, lang }) }),
  // OAuth
  generateOAuthURL: (data: { proxy_url?: string; redirect_uri?: string }) =>
    request<OAuthURLResponse>('/oauth/generate-auth-url', { method: 'POST', body: JSON.stringify(data) }),
  exchangeOAuthCode: (data: { session_id: string; code: string; state: string; name?: string; proxy_url?: string }) =>
    request<OAuthExchangeResponse>('/oauth/exchange-code', { method: 'POST', body: JSON.stringify(data) }),
  updateOAuthAccount: (id: number, data: UpdateOAuthAccountRequest) =>
    request<OAuthExchangeResponse>(`/accounts/${id}/oauth/exchange-code`, { method: 'POST', body: JSON.stringify(data) }),
}

export interface ProxyRow {
  id: number
  url: string
  label: string
  enabled: boolean
  created_at: string
  test_ip: string
  test_location: string
  test_latency_ms: number
}

export interface ProxyTestResult {
  success: boolean
  ip?: string
  country?: string
  region?: string
  city?: string
  isp?: string
  latency_ms?: number
  location?: string
  error?: string
}
