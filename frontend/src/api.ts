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
  OpenAIResponsesBalanceResponse,
  AddGrokAccountRequest,
  UpdateGrokAccountRequest,
  AddAntigravityAccountRequest,
  AntigravityCreateResponse,
  UpdateAntigravityAccountRequest,
  AntigravityImportRequest,
  AntigravityImportResponse,
  AntigravityOAuthStartRequest,
  AntigravityOAuthStartResponse,
  AntigravityOAuthStatusResponse,
  AntigravityOAuthCompleteRequest,
  AntigravityOAuthCompleteResponse,
  AntigravityAccountState,
  AntigravityStateSyncResponse,
  AntigravityCapabilityProbeResponse,
  BatchUpdateGrokModelsRequest,
  BatchUpdateGrokModelsResponse,
  FetchGrokModelsResponse,
  GrokDeviceStartRequest,
  GrokDeviceStartResponse,
  GrokDevicePollRequest,
  GrokDevicePollResponse,
  GrokSSOImportRequest,
  GrokSSOImportResponse,
  GrokBatchImportRequest,
  GrokBatchImportResponse,
  GrokAccountState,
  GrokStateSyncResponse,
  GrokCapabilityProbeResponse,
  AdminErrorResponse,
  APIKeysResponse,
  APIKeyTokenStat,
  APIKeyAccountStatsResponse,
  APIKeyScopeUsageItem,
  APIKeyLimits,
  APIKeyModelRequestUsage,
  APIKeyScopeSummaryItem,
  AccountsResponse,
  AccountAnalysisResponse,
  AccountsPageParams,
  AccountsPageResponse,
  AccountPageStatsResponse,
  AccountLiveStateResponse,
  ChartAggregation,
  CreateAccountResponse,
  CreateAPIKeyResponse,
  CreateAPIKeyRequest,
  CreatePromptFilterNewAPIBindingRequest,
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
  InviteRecipientsCheckResponse,
  InviteEligibilityResponse,
  InviteGuidePlan,
  InviteTrackingResponse,
  MessageResponse,
  ModelSyncResponse,
  RefreshAllModelsResponse,
  ProxyRiskScoreSnapshot,
  ProxyRiskScoringProfile,
  ProxyRiskScoringJob,
  PromptLogRetention,
  ModelPricingOverride,
	OfficialPricingSyncConfig,
	OfficialPricingSyncResult,
  ModelsResponse,
  OAuthExchangeResponse,
  OAuthURLResponse,
  ClaudeAuthURLResponse,
  ClaudeExchangeCodeRequest,
  ClaudeImportTokenRequest,
  ClaudeCredentialExportEntry,
  ClaudeImportBundleResponse,
  ClaudeAddAccountResponse,
  OpsErrorSummary,
  OpsOverviewResponse,
  PromptFilterLog,
  PromptFilterLogsResponse,
	PromptPolicyIncidentDetailResponse,
	PromptPolicyAuditHealth,
	PromptPolicyIncidentsResponse,
  PromptFilterNewAPIBinding,
  PromptFilterNewAPIBindingsResponse,
  PromptFilterRulePatternTestResponse,
  PromptFilterRulesResponse,
  PromptFilterTestResponse,
  PromptReviewTestRequest,
  PromptReviewTestResponse,
  PromptReviewAPIKeysResponse,
  PublicAPIKeyUsageResponse,
  RecycleBinAccountsResponse,
  ResetCreditsDetailResponse,
  WhamDailyUsageResponse,
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
  UpdatePromptFilterNewAPIBindingRequest,
  UpdateOAuthAccountRequest,
  UpdateOpenAIResponsesAccountRequest,
  UsageLogsResponse,
  UsageLogsPagedResponse,
  UsageStats,
  AccountGroup,
  AccountRow,
  AccountGroupsResponse,
  AccountOperationSelector,
  AccountHealthBarsResponse,
  BatchUpdateAccountsRequest,
  BackgroundUploadResponse,
  CreateAccountGroupRequest,
  UpdateAccountGroupRequest,
  UpstreamChannel,
  ClaudeGlobalConfig,
  VisibleChannelsSettings,
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

export class AdminAPIError extends Error {
  readonly status: number

  constructor(status: number, message: string) {
    super(message)
    this.name = 'AdminAPIError'
    this.status = status
  }
}

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
    throw new AdminAPIError(res.status, extractAdminErrorMessage(body, res.status))
  }

  if (res.status === 204) {
    return undefined as T
  }
  const text = await res.text()
  return (text ? JSON.parse(text) : undefined) as T
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

/** 下载物：blob + 服务端给出的文件名与实际导出数量（响应头可能缺省）。 */
export type NamedBlob = { blob: Blob; filename: string; count?: number }

/** parseContentDispositionFilename 取 Content-Disposition 里的 filename，取不到返回空串。 */
function parseContentDispositionFilename(header: string | null): string {
  if (!header) return ''
  // RFC 5987 的 filename*=UTF-8''… 优先，其次普通 filename="…"。
  const encoded = /filename\*=(?:UTF-8|utf-8)''([^;]+)/i.exec(header)
  if (encoded?.[1]) {
    try {
      return decodeURIComponent(encoded[1].trim())
    } catch {
      // 编码不合法时退回普通 filename
    }
  }
  const plain = /filename="?([^";]+)"?/i.exec(header)
  return plain?.[1]?.trim() ?? ''
}

function parseOptionalCountHeader(header: string | null): number | undefined {
  if (!header || !/^\d+$/.test(header.trim())) return undefined
  const count = Number(header.trim())
  return Number.isSafeInteger(count) && count >= 0 ? count : undefined
}

async function requestNamedBlob(path: string, options: RequestInit = {}): Promise<NamedBlob> {
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

  return {
    blob: await res.blob(),
    filename: parseContentDispositionFilename(res.headers.get('Content-Disposition')),
    count: parseOptionalCountHeader(res.headers.get('X-Export-Count')),
  }
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

export type UsageLogQueryParams = {
  start: string
  end: string
  email?: string
  q?: string
  model?: string
  endpoint?: string
  apiKeyId?: string
  accountId?: string
  fast?: string
  stream?: string
  compact?: string
  hasCompactionHistory?: string
  channel?: string
  status?: string
  errorOnly?: string
  errorKind?: string
  retry?: string
  viaWebsocket?: string
  includeCanceled?: string
}

export function buildUsageLogSearchParams(params: UsageLogQueryParams) {
  const search = new URLSearchParams()
  search.set('start', params.start)
  search.set('end', params.end)
  if (params.email) search.set('email', params.email)
  if (params.q) search.set('q', params.q)
  if (params.model) search.set('model', params.model)
  if (params.endpoint) search.set('endpoint', params.endpoint)
  if (params.apiKeyId) search.set('api_key_id', params.apiKeyId)
  if (params.accountId) search.set('account_id', params.accountId)
  if (params.fast) search.set('fast', params.fast)
  if (params.stream) search.set('stream', params.stream)
  if (params.compact) search.set('compact', params.compact)
  if (params.hasCompactionHistory) search.set('has_compaction_history', params.hasCompactionHistory)
  if (params.channel) search.set('channel', params.channel)
  if (params.status) search.set('status', params.status)
  if (params.errorOnly) search.set('error_only', params.errorOnly)
  if (params.errorKind) search.set('error_kind', params.errorKind)
  if (params.retry) search.set('retry', params.retry)
  if (params.viaWebsocket) search.set('via_websocket', params.viaWebsocket)
  if (params.includeCanceled) search.set('include_canceled', params.includeCanceled)
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
  // channel is a first-class upstream provider filter; omit for all accounts.
  // view: 'lite' — 只返回身份/绑定字段,跳过用量富化(代理绑定弹窗等场景)。
  getAccounts: (params: { channel?: UpstreamChannel; view?: 'lite' } = {}) => {
    const searchParams = new URLSearchParams()
    if (params.channel) searchParams.set('channel', params.channel)
    if (params.view) searchParams.set('view', params.view)
    const qs = searchParams.toString()
    return request<AccountsResponse>(`/accounts${qs ? `?${qs}` : ''}`)
  },
  getAccountsPage: (params: AccountsPageParams, signal?: AbortSignal) => {
    const searchParams = new URLSearchParams({
      view: 'page',
      page: String(params.page),
      page_size: String(params.pageSize),
    })
    if (params.channel) searchParams.set('channel', params.channel)
    if (params.search?.trim()) searchParams.set('search', params.search.trim())
    if (params.status && params.status !== 'all') searchParams.set('status', params.status)
    if (params.plan && params.plan !== 'all') searchParams.set('plan', params.plan)
    if (params.authKind && params.authKind !== 'all') searchParams.set('auth_kind', params.authKind)
    if (params.tag) searchParams.set('tag', params.tag)
    if (params.emailDomain) searchParams.set('email_domain', params.emailDomain)
    if (params.groupInclude?.length) searchParams.set('group_include', params.groupInclude.join(','))
    if (params.groupExclude?.length) searchParams.set('group_exclude', params.groupExclude.join(','))
    if (params.ungrouped) searchParams.set('ungrouped', 'true')
    if (params.healthTier) searchParams.set('health_tier', params.healthTier)
    if (params.proxyUrl) searchParams.set('proxy_url', params.proxyUrl)
    if (params.proxyFilter && params.proxyFilter !== 'all') searchParams.set('proxy_filter', params.proxyFilter)
    if (params.sort) searchParams.set('sort', params.sort)
    if (params.order) searchParams.set('order', params.order)
    return request<AccountsPageResponse>(`/accounts?${searchParams.toString()}`, { signal })
  },
  getAccountAnalysis: (channel: 'codex' | 'grok' | 'antigravity' | 'claude' = 'codex', signal?: AbortSignal) =>
    request<AccountAnalysisResponse>(`/accounts/analysis?channel=${channel}`, { signal }),
  getAccountPageStats: (ids: number[], signal?: AbortSignal) => {
    const query = new URLSearchParams({ ids: ids.join(',') })
    return request<AccountPageStatsResponse>(`/accounts/page-stats?${query}`, { signal })
  },
  getAccountLiveState: (ids: number[], signal?: AbortSignal) => {
    const query = new URLSearchParams({ ids: ids.join(',') })
    return request<AccountLiveStateResponse>(`/accounts/live?${query}`, { signal })
  },
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
  getOpenAIResponsesBalance: (id: number, signal?: AbortSignal, force = false) =>
    request<OpenAIResponsesBalanceResponse>(
      `/accounts/${id}/openai-responses/balance${force ? '?refresh=1' : ''}`,
      { signal, timeoutMs: 25_000 },
    ),
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
  fetchAntigravityModels: (data: AddAntigravityAccountRequest) =>
    request<{ models: string[] }>('/accounts/antigravity/models', {
      method: 'POST',
      body: JSON.stringify(data),
    }),
  batchUpdateAntigravityModels: (data: { ids: number[]; models: string[] }) =>
    request<{ success: number; failed: number; models: string[] }>('/accounts/antigravity/batch-models', {
      method: 'POST',
      body: JSON.stringify(data),
    }),
  addAntigravityAccount: (data: AddAntigravityAccountRequest) =>
    request<AntigravityCreateResponse>('/accounts/antigravity', {
      method: 'POST',
      body: JSON.stringify(data),
      timeoutMs: 120_000,
    }),
  importAntigravityAccounts: (data: AntigravityImportRequest) =>
    request<AntigravityImportResponse>('/accounts/antigravity/import', {
      method: 'POST',
      body: JSON.stringify(data),
      timeoutMs: 180_000,
    }),
  updateAntigravityAccount: (id: number, data: UpdateAntigravityAccountRequest) =>
    request<MessageResponse>(`/accounts/${id}/antigravity`, {
      method: 'PATCH',
      body: JSON.stringify(data),
      timeoutMs: 120_000,
    }),
  refreshAntigravityAccount: (id: number) =>
    request<MessageResponse>(`/accounts/${id}/antigravity/refresh`, {
      method: 'POST',
      timeoutMs: 120_000,
    }),
  refreshAntigravityQuota: (id: number) =>
    request<MessageResponse>(`/accounts/${id}/antigravity/quota`, {
      method: 'POST',
      timeoutMs: 120_000,
    }),
  getAntigravityAccountState: (id: number, signal?: AbortSignal) =>
    request<AntigravityAccountState>(`/accounts/${id}/antigravity/state`, { signal }),
  syncAntigravityAccountState: (id: number) =>
    request<AntigravityStateSyncResponse>(`/accounts/${id}/antigravity/sync`, {
      method: 'POST',
      timeoutMs: 120_000,
    }),
  probeAntigravityAccountCapabilities: (id: number) =>
    request<AntigravityCapabilityProbeResponse>(`/accounts/${id}/antigravity/capabilities/probe`, {
      method: 'POST',
      timeoutMs: 45_000,
    }),
  startAntigravityOAuth: (data: AntigravityOAuthStartRequest = {}) =>
    request<AntigravityOAuthStartResponse>('/accounts/antigravity/oauth/start', {
      method: 'POST',
      body: JSON.stringify(data),
      timeoutMs: 20_000,
    }),
  getAntigravityOAuthStatus: (sessionId: string, signal?: AbortSignal) =>
    request<AntigravityOAuthStatusResponse>(
      `/accounts/antigravity/oauth/status?session_id=${encodeURIComponent(sessionId)}`,
      { signal, timeoutMs: 10_000 },
    ),
  completeAntigravityOAuth: (data: AntigravityOAuthCompleteRequest) =>
    request<AntigravityOAuthCompleteResponse>('/accounts/antigravity/oauth/complete', {
      method: 'POST',
      body: JSON.stringify(data),
      timeoutMs: 15_000,
    }),
  cancelAntigravityOAuth: (sessionId: string) =>
    request<void>(`/accounts/antigravity/oauth/${encodeURIComponent(sessionId)}`, {
      method: 'DELETE',
    }),
  // Claude Code OAuth：第一步取授权 URL（服务端暂存 state→verifier）。
  generateClaudeAuthURL: () =>
    request<ClaudeAuthURLResponse>('/accounts/claude/oauth/auth-url', {
      method: 'POST',
      body: JSON.stringify({}),
      timeoutMs: 15_000,
    }),
  // 第二步：用 state+code 换取 token 并入库（可选从代理池分配代理）。
  exchangeClaudeOAuthCode: (data: ClaudeExchangeCodeRequest) =>
    request<ClaudeAddAccountResponse>('/accounts/claude/oauth/exchange-code', {
      method: 'POST',
      body: JSON.stringify(data),
      timeoutMs: 90_000,
    }),
  // CLI 直导：吃 cmd/claude_login -out 产出的 token JSON。
  importClaudeToken: (data: ClaudeImportTokenRequest) =>
    request<ClaudeAddAccountResponse>('/accounts/claude/import', {
      method: 'POST',
      body: JSON.stringify(data),
      timeoutMs: 20_000,
    }),
  /** Import a versioned Claude credential object or bundle. */
  importClaudeCredentialBundle: (
    data: ClaudeCredentialExportEntry | ClaudeCredentialExportEntry[] | { accounts: ClaudeCredentialExportEntry[] },
  ) =>
    request<ClaudeAddAccountResponse | ClaudeImportBundleResponse>('/accounts/claude/import', {
      method: 'POST',
      body: JSON.stringify(data),
      timeoutMs: 120_000,
    }),
  /** Download one Claude JSON credential or a ZIP for multiple accounts. */
  exportClaudeAccounts: (ids?: number[], filter: 'all' | 'healthy' = 'all', format: 'auto' | 'json' | 'zip' = 'auto') => {
    const params = new URLSearchParams({ filter, format })
    if (ids && ids.length > 0) params.set('ids', ids.join(','))
    return requestNamedBlob(`/accounts/claude/export?${params.toString()}`)
  },
  refreshClaudeModels: (id: number) =>
    request<{ message: string; models: string[]; count: number }>(`/accounts/${id}/claude/models`, {
      method: 'POST',
      timeoutMs: 30_000,
    }),
  refreshAllClaudeModels: () =>
    request<{ message: string; refreshed: number; failed: number; model_count: number }>('/accounts/claude/models/refresh', {
      method: 'POST',
      timeoutMs: 60_000,
    }),
  refreshAllModels: () =>
    request<RefreshAllModelsResponse>('/models/refresh-all', {
      method: 'POST',
      timeoutMs: 130_000,
    }),
  batchUpdateGrokModels: (data: BatchUpdateGrokModelsRequest) =>
    request<BatchUpdateGrokModelsResponse>('/accounts/grok/batch-models', {
      method: 'POST',
      body: JSON.stringify(data),
    }),
  getGrokAccountState: (id: number, signal?: AbortSignal) =>
    request<GrokAccountState>(`/accounts/${id}/grok/state`, { signal }),
  syncGrokAccountState: (id: number) =>
    request<GrokStateSyncResponse>(`/accounts/${id}/grok/sync`, {
      method: 'POST',
      timeoutMs: 120_000,
    }),
  probeGrokAccountCapabilities: (id: number) =>
    request<GrokCapabilityProbeResponse>(`/accounts/${id}/grok/capabilities/probe`, {
      method: 'POST',
      timeoutMs: 180_000,
    }),
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
  getAccount: (id: number, signal?: AbortSignal) =>
    request<AccountRow>(`/accounts/${id}`, { signal }),
  forceUsageProbe: () =>
    request<{ triggered: boolean; concurrency: number; reason?: string; mode?: string }>(`/accounts/usage/probe`, { method: 'POST' }),
  refreshAccountUsage: (id: number) =>
    request<{
      refreshed: boolean
      usage_percent_5h?: number
      usage_percent_7d?: number
      usage_percent_spark?: number
      reset_5h_at?: string
      reset_7d_at?: string
      reset_spark_at?: string
      claude_usage_probe_at?: string
      claude_usage_probe_error?: string
      claude_usage_windows?: import('./types').ClaudeUsageWindow[]
      claude_usage_windows_probed?: boolean
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
  updateAccountModelCooldownPolicy: (
    id: number,
    data: {
      mode?: 'off' | 'fixed' | 'adaptive' | null
      seconds?: number | null
      backoff_enabled?: boolean | null
    },
  ) =>
    request<{
      message: string
      mode_effective: 'off' | 'fixed' | 'adaptive'
      seconds_effective: number
      backoff_enabled_effective: boolean
    }>(`/accounts/${id}/model-cooldown-policy`, {
      method: 'PATCH',
      body: JSON.stringify(data),
    }),
  clearAccountModelCooldown: (id: number, model: string) =>
    request<MessageResponse>(`/accounts/${id}/model-cooldowns/${encodeURIComponent(model)}`, {
      method: 'DELETE',
    }),
  clearAllAccountModelCooldowns: (id: number) =>
    request<{ message: string; cleared: number }>(`/accounts/${id}/model-cooldowns`, {
      method: 'DELETE',
    }),
  // usage_refreshed 表示重置后的用量探针是否在响应前跑完；false 时调用方应稍后补刷一次。
  resetCredits: (id: number) =>
    request<{
      message: string
      rate_limit_reset_credits?: number
      windows_reset?: number
      usage_refreshed?: boolean
      status?: string
    }>(`/accounts/${id}/reset-credits`, { method: 'POST' }),
  getResetCredits: (id: number) =>
    request<ResetCreditsDetailResponse>(`/accounts/${id}/reset-credits`),
  // 官方结算用量历史。默认读本地快照；refresh 时先打上游回补保留窗口再返回。
  getWhamDailyUsage: (id: number, days = 30, refresh = false) => {
    const search = new URLSearchParams({ days: String(days) })
    if (refresh) search.set('refresh', '1')
    return request<WhamDailyUsageResponse>(`/accounts/${id}/wham-daily-usage?${search.toString()}`)
  },
  getAccountHealthBars: (ids: number[] = []) => {
    const query = ids.length > 0 ? `?ids=${ids.join(',')}` : ''
    return request<AccountHealthBarsResponse>(`/accounts/health-bars${query}`)
  },
  sendInvite: (id: number, data: { emails?: string[]; emails_text?: string; program_id?: string; entrypoint?: string; proxy_url?: string; max_emails?: number }) =>
    request<InviteResponse>(`/accounts/${id}/invite`, { method: 'POST', body: JSON.stringify(data) }),
  checkInviteRecipients: (emails: string[], signal?: AbortSignal) =>
    request<InviteRecipientsCheckResponse>('/accounts/invite/recipients/check', {
      method: 'POST',
      body: JSON.stringify({ emails }),
      signal,
    }),
  // refresh=1 绕过网关的资格/记录缓存直连上游，用于手动刷新与发送邀请后的重拉。
  getInviteEligibility: (id: number, params?: { program_id?: string; entrypoint?: string; proxy_url?: string; refresh?: boolean }) => {
    const search = new URLSearchParams()
    if (params?.program_id) search.set('program_id', params.program_id)
    if (params?.entrypoint) search.set('entrypoint', params.entrypoint)
    if (params?.proxy_url) search.set('proxy_url', params.proxy_url)
    if (params?.refresh) search.set('refresh', '1')
    const qs = search.toString()
    return request<InviteEligibilityResponse>(`/accounts/${id}/invite/eligibility${qs ? `?${qs}` : ''}`)
  },
  getInviteTracking: (id: number, params?: { program_id?: string; period?: string; limit?: number; proxy_url?: string; refresh?: boolean }) => {
    const search = new URLSearchParams()
    if (params?.program_id) search.set('program_id', params.program_id)
    if (params?.period) search.set('period', params.period)
    if (typeof params?.limit === 'number') search.set('limit', String(params.limit))
    if (params?.proxy_url) search.set('proxy_url', params.proxy_url)
    if (params?.refresh) search.set('refresh', '1')
    const qs = search.toString()
    return request<InviteTrackingResponse>(`/accounts/${id}/invite/tracking${qs ? `?${qs}` : ''}`)
  },
  // 导入后的邀请收益评估。emails 是可用受邀邮箱数，传了就按「单次收益高的号优先」
  // 做贪心分配；不传表示不限，建议次数等于各账号的剩余奖励次数。
  getInviteGuidePlan: (ids: number[], emails?: number) => {
    const search = new URLSearchParams({ ids: ids.join(',') })
    if (typeof emails === 'number' && emails > 0) search.set('emails', String(emails))
    return request<InviteGuidePlan>(`/accounts/invite/plan?${search.toString()}`)
  },
  probeInviteGuidePlan: (ids: number[]) =>
    request<{ queued: number; skipped: number }>('/accounts/invite/plan/probe', {
      method: 'POST',
      body: JSON.stringify({ ids }),
    }),
  getVisibleChannels: () => request<VisibleChannelsSettings>('/settings/visible-channels'),
  updateVisibleChannels: (channels: readonly UpstreamChannel[]) =>
    request<VisibleChannelsSettings>('/settings/visible-channels', {
      method: 'PUT',
      body: JSON.stringify({ channels }),
    }),
  getInviteGuideSettings: () => request<{ enabled: boolean }>('/settings/invite-guide'),
  updateInviteGuideSettings: (enabled: boolean) =>
    request<{ enabled: boolean }>('/settings/invite-guide', {
      method: 'PUT',
      body: JSON.stringify({ enabled }),
    }),
  batchResetStatus: (ids: number[]) =>
    request<{ message: string; success: number; failed: number }>('/accounts/batch-reset-status', { method: 'POST', body: JSON.stringify({ ids }) }),
  batchDeleteAccounts: (ids: number[]) =>
    request<{ message: string; deleted: number; success: number; failed: number }>('/accounts/batch-delete', { method: 'POST', body: JSON.stringify({ ids }) }),
  batchRefreshAccounts: (target: number[] | AccountOperationSelector) =>
    request<{ message: string; success: number; failed: number }>('/accounts/batch-refresh', {
      method: 'POST',
      body: JSON.stringify(Array.isArray(target) ? { ids: target } : { selector: target }),
    }),
  getAccountUsage: (id: number, days?: number) => {
    const search = new URLSearchParams()
    if (typeof days === 'number') search.set('days', String(days))
    const qs = search.toString()
    return request<AccountUsageDetail>(`/accounts/${id}/usage${qs ? `?${qs}` : ''}`)
  },
  updateAccountCredit: (id: number, data: { credit_enabled: boolean; credit_skip_usage_window: boolean }) =>
    request<MessageResponse>(`/accounts/${id}/credit`, { method: 'PATCH', body: JSON.stringify(data) }),
  getHealth: () => request<HealthResponse>('/health'),
  getPromptFilterNewAPIBindings: () =>
    request<PromptFilterNewAPIBindingsResponse>('/prompt-filter/newapi-bindings'),
  getPromptFilterNewAPIBinding: (apiKeyId: number) =>
    request<PromptFilterNewAPIBinding>(`/prompt-filter/newapi-bindings/${apiKeyId}`),
  createPromptFilterNewAPIBinding: (data: CreatePromptFilterNewAPIBindingRequest) =>
    request<PromptFilterNewAPIBinding>('/prompt-filter/newapi-bindings', {
      method: 'POST',
      body: JSON.stringify(data),
    }),
  updatePromptFilterNewAPIBinding: (apiKeyId: number, data: UpdatePromptFilterNewAPIBindingRequest) =>
    request<PromptFilterNewAPIBinding>(`/prompt-filter/newapi-bindings/${apiKeyId}`, {
      method: 'PATCH',
      body: JSON.stringify(data),
    }),
  generatePromptFilterNewAPIBindingSecret: (apiKeyId: number, graceSeconds: number) =>
    request<PromptFilterNewAPIBinding>(`/prompt-filter/newapi-bindings/${apiKeyId}/secret/generate`, {
      method: 'POST',
      body: JSON.stringify({ grace_seconds: graceSeconds }),
    }),
  replacePromptFilterNewAPIBindingSecret: (apiKeyId: number, secret: string, graceSeconds: number) =>
    request<PromptFilterNewAPIBinding>(`/prompt-filter/newapi-bindings/${apiKeyId}/secret`, {
      method: 'PUT',
      body: JSON.stringify({ secret, grace_seconds: graceSeconds }),
    }),
  deletePromptFilterNewAPIBinding: (apiKeyId: number) =>
    request<MessageResponse>(`/prompt-filter/newapi-bindings/${apiKeyId}`, { method: 'DELETE' }),
  getOpsOverview: (signal?: AbortSignal) => request<OpsOverviewResponse>('/ops/overview', { signal }),
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
  getUsageStats: (params: {
    start?: string
    end?: string
    channel?: string
    detail?: 'summary'
    signal?: AbortSignal
  } = {}) => {
    const searchParams = new URLSearchParams()
    if (params.start) searchParams.set('start', params.start)
    if (params.end) searchParams.set('end', params.end)
    if (params.channel) searchParams.set('channel', params.channel)
    if (params.detail) searchParams.set('detail', params.detail)
    const qs = searchParams.toString()
    return request<UsageStats>(qs ? `/usage/stats?${qs}` : '/usage/stats', {
      signal: params.signal,
    })
  },
  getAPIKeyTokenStats: (params: { start?: string; end?: string; signal?: AbortSignal } = {}) => {
    const searchParams = new URLSearchParams()
    if (params.start) searchParams.set('start', params.start)
    if (params.end) searchParams.set('end', params.end)
    const qs = searchParams.toString()
    return request<{ items: APIKeyTokenStat[] }>(
      qs ? `/usage/api-keys?${qs}` : '/usage/api-keys',
      { signal: params.signal },
    )
  },
  getAPIKeyAccountStats: (id: number, params: { start?: string; end?: string; signal?: AbortSignal } = {}) => {
    const searchParams = new URLSearchParams()
    if (params.start) searchParams.set('start', params.start)
    if (params.end) searchParams.set('end', params.end)
    const qs = searchParams.toString()
    return request<APIKeyAccountStatsResponse>(
      qs ? `/usage/api-keys/${id}/accounts?${qs}` : `/usage/api-keys/${id}/accounts`,
      { signal: params.signal },
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
  getUsageLogsPaged: (params: UsageLogQueryParams & { page: number; pageSize?: number }) => {
    const searchParams = buildUsageLogSearchParams(params)
    searchParams.set('page', String(params.page))
    if (params.pageSize) searchParams.set('page_size', String(params.pageSize))
    return request<UsageLogsPagedResponse>(`/usage/logs?${searchParams.toString()}`)
  },
  getUsageLogsErrorSummary: (params: UsageLogQueryParams) => {
    const searchParams = buildUsageLogSearchParams(params)
    return request<OpsErrorSummary>(`/usage/logs/error-summary?${searchParams.toString()}`)
  },
  getChartData: (params: {
    start: string
    end: string
    bucketMinutes: number
    channel?: string
    signal?: AbortSignal
  }) => {
    const searchParams = new URLSearchParams()
    searchParams.set('start', params.start)
    searchParams.set('end', params.end)
    searchParams.set('bucket_minutes', String(params.bucketMinutes))
    if (params.channel) searchParams.set('channel', params.channel)
    return request<ChartAggregation>(`/usage/chart-data?${searchParams.toString()}`, {
      signal: params.signal,
    })
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
    request<MessageResponse & { limits?: APIKeyLimits }>(`/keys/${id}`, { method: 'PATCH', body: JSON.stringify(data) }),
  resetAPIKeyQuota: (id: number) =>
    request<MessageResponse>(`/keys/${id}/reset-quota`, { method: 'POST' }),
  resetAllAPIKeyQuotas: () =>
    request<MessageResponse & { reset_count: number }>('/keys/reset-all-quotas', { method: 'POST' }),
  // 分组 / 账号维度限额的当前用量（issue #439）。
  getAPIKeyScopeUsage: (id: number) =>
    request<{ items: APIKeyScopeUsageItem[] }>(`/keys/${id}/scope-usage`),
  getAPIKeyModelRequestUsage: (id: number) =>
    request<{ model_request_usage: APIKeyModelRequestUsage[] }>(`/keys/${id}/model-request-usage`),
  // 列表页用的全量概览：一次拿到所有 Key 的 scope 预算占比。
  getAPIKeysScopeSummary: () =>
    request<{ summary: Record<string, APIKeyScopeSummaryItem[]> }>('/keys-scope-summary'),
  // 重置某条 scope 的累计额度（累计额度不随时间回落，必须显式重置）。
  resetAPIKeyScopeQuota: (id: number, data: { scope_type: 'group' | 'account'; scope_id: number }) =>
    request<MessageResponse>(`/keys/${id}/scope-quota/reset`, {
      method: 'POST',
      body: JSON.stringify(data),
    }),
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
  getClaudeConfig: () =>
    request<ClaudeGlobalConfig>('/settings/claude-config'),
  updateClaudeConfig: (data: ClaudeGlobalConfig) =>
    request<{ message: string } & ClaudeGlobalConfig>('/settings/claude-config', {
      method: 'PUT',
      body: JSON.stringify(data),
    }),
  syncClaudeCLIVersion: () =>
    request<{
      fetched_version: string
      effective_version: string
      builtin_version: string
      updated: boolean
      accounts_refreshed: number
      warning?: string
    }>('/settings/claude-config/cli-version/sync', { method: 'POST' }),
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
  getPromptFilterLogs: (params: number | { page?: number; pageSize?: number; limit?: number; source?: string; action?: string; endpoint?: string; model?: string; apiKeyId?: string; q?: string; reviewed?: boolean; reviewResult?: string } = 100) => {
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
      if (typeof params.reviewed === 'boolean') search.set('reviewed', String(params.reviewed))
      if (params.reviewResult) search.set('review_result', params.reviewResult)
    }
    return request<PromptFilterLogsResponse>(`/prompt-filter/logs?${search.toString()}`)
  },
  clearPromptFilterLogs: (params: { reviewed?: boolean; source?: 'local_filter' } = {}) => {
    const search = new URLSearchParams()
    if (typeof params.reviewed === 'boolean') search.set('reviewed', String(params.reviewed))
    if (params.source) search.set('source', params.source)
    const suffix = search.size > 0 ? `?${search.toString()}` : ''
    return request<MessageResponse>(`/prompt-filter/logs${suffix}`, { method: 'DELETE' })
  },
  matchPromptFilterLog: (params: { at: string; endpoint?: string; apiKeyId?: number; source?: string }) => {
    const search = new URLSearchParams()
    search.set('at', params.at)
    if (params.endpoint) search.set('endpoint', params.endpoint)
    if (params.apiKeyId) search.set('api_key_id', String(params.apiKeyId))
    if (params.source) search.set('source', params.source)
		return request<{ found: boolean; log: PromptFilterLog | null; legacy_inferred: boolean }>(`/prompt-filter/logs/match?${search.toString()}`)
	},
	getPromptPolicyIncidents: (params: { page?: number; pageSize?: number; endpoint?: string; model?: string; apiKeyId?: string; accountId?: string; evaluationState?: string; outcome?: string; localComparison?: string; localMiss?: boolean; q?: string } = {}) => {
		const search = new URLSearchParams()
		search.set('page', String(params.page || 1))
		search.set('page_size', String(params.pageSize || 20))
		if (params.endpoint) search.set('endpoint', params.endpoint)
		if (params.model) search.set('model', params.model)
		if (params.apiKeyId) search.set('api_key_id', params.apiKeyId)
		if (params.accountId) search.set('account_id', params.accountId)
		if (params.evaluationState) search.set('evaluation_state', params.evaluationState)
		if (params.outcome) search.set('outcome', params.outcome)
		if (params.localComparison) search.set('local_comparison', params.localComparison)
		if (params.localMiss !== undefined) search.set('local_miss', String(params.localMiss))
		if (params.q) search.set('q', params.q)
		return request<PromptPolicyIncidentsResponse>(`/prompt-policy/incidents?${search.toString()}`)
	},
	getPromptPolicyIncident: (incidentId: string) =>
		request<PromptPolicyIncidentDetailResponse>(`/prompt-policy/incidents/${encodeURIComponent(incidentId)}`),
	getPromptPolicyAuditHealth: () =>
		request<PromptPolicyAuditHealth>('/prompt-policy/incidents/health'),
	getPromptLogRetention: () => request<PromptLogRetention>('/prompt-filter/retention'),
	updatePromptLogRetention: (retentionDays: number) =>
		request<PromptLogRetention>('/prompt-filter/retention', { method: 'PUT', body: JSON.stringify({ retention_days: retentionDays }) }),
	runPromptLogRetention: () =>
		request<{ started: boolean; retention_days: number }>('/prompt-filter/retention/run', { method: 'POST' }),
	clearPromptPolicyIncidents: () =>
		request<MessageResponse>('/prompt-policy/incidents', { method: 'DELETE' }),
	deletePromptPolicyIncident: (incidentId: string) =>
		request<MessageResponse>(`/prompt-policy/incidents/${encodeURIComponent(incidentId)}`, { method: 'DELETE' }),
	getPromptRiskProfiles: (params: { page?: number; pageSize?: number; subjectType?: string; platform?: string; riskLevel?: string; apiKeyId?: string; accountId?: string; minScore?: string; q?: string; lockedOnly?: boolean; cyOnly?: boolean; activityState?: string } = {}) => {
		const search = new URLSearchParams()
		search.set('page', String(params.page || 1))
		search.set('page_size', String(params.pageSize || 20))
		if (params.subjectType) search.set('subject_type', params.subjectType)
		if (params.platform) search.set('platform', params.platform)
		if (params.riskLevel) search.set('risk_level', params.riskLevel)
		if (params.apiKeyId) search.set('api_key_id', params.apiKeyId)
		if (params.accountId) search.set('account_id', params.accountId)
		if (params.minScore) search.set('min_score', params.minScore)
		if (params.q) search.set('q', params.q)
		if (params.lockedOnly) search.set('locked_only', 'true')
		if (params.cyOnly) search.set('cy_only', 'true')
		if (params.activityState) search.set('activity_state', params.activityState)
		return request<import('./types').PromptRiskProfilesResponse>(`/prompt-policy/risk-profiles?${search.toString()}`)
	},
	getPromptRiskProfile: (subjectType: string, subjectKey: string, eventPage = 1, eventPageSize = 20, trustEventPage = 1, trustEventPageSize = 20) =>
		request<import('./types').PromptRiskProfileDetailResponse>(`/prompt-policy/risk-profiles/${encodeURIComponent(subjectType)}/${encodeURIComponent(subjectKey)}?event_page=${eventPage}&event_page_size=${eventPageSize}&trust_event_page=${trustEventPage}&trust_event_page_size=${trustEventPageSize}`),
	upsertPromptRiskTrust: (subjectType: string, subjectKey: string, data: { duration_hours: number; risk_threshold: number; reason: string }) =>
		request<{ policy: import('./types').PromptRiskTrustPolicy }>(`/prompt-policy/risk-profiles/${encodeURIComponent(subjectType)}/${encodeURIComponent(subjectKey)}/trust`, { method: 'PUT', body: JSON.stringify(data) }),
	revokePromptRiskTrust: (subjectType: string, subjectKey: string) =>
		request<{ policy: import('./types').PromptRiskTrustPolicy }>(`/prompt-policy/risk-profiles/${encodeURIComponent(subjectType)}/${encodeURIComponent(subjectKey)}/trust`, { method: 'DELETE' }),
	unlockPromptConversation: (lockKey: string, reason = '管理员主动解锁', scope: 'conversation' | 'user_cooldown' = 'conversation') =>
		request<{ lock: import('./types').PromptConversationLock; scope: string; unlocked_count: number }>(`/prompt-policy/conversation-locks/${encodeURIComponent(lockKey)}/unlock`, { method: 'POST', body: JSON.stringify({ reason, scope }) }),
  testPromptFilter: (data: { text: string; endpoint?: string; model?: string }) =>
    request<PromptFilterTestResponse>('/prompt-filter/test', { method: 'POST', body: JSON.stringify(data) }),
  testPromptReview: (data: PromptReviewTestRequest) =>
    request<PromptReviewTestResponse>('/prompt-filter/review/test', { method: 'POST', body: JSON.stringify(data) }),
  listPromptReviewModels: (data: { base_url?: string; api_key?: string; timeout_seconds?: number }) =>
    request<{ endpoint: string; models: string[] }>('/prompt-filter/review/models', { method: 'POST', body: JSON.stringify(data) }),
  getPromptReviewAPIKeys: () =>
    request<PromptReviewAPIKeysResponse>('/prompt-filter/review/keys'),
  deletePromptReviewAPIKey: (keyID: string) =>
    request<PromptReviewAPIKeysResponse>(`/prompt-filter/review/keys/${encodeURIComponent(keyID)}`, { method: 'DELETE' }),
  listPromptReviewProfiles: () =>
    request<import('./types').PromptReviewProfilesResponse>('/prompt-filter/review/profiles'),
  savePromptReviewProfile: (data: {
    id?: string
    name: string
    base_url: string
    model: string
    request_mode: string
    api_key?: string
    adapter_json: string
    timeout_seconds: number
  }) =>
    request<import('./types').PromptReviewProfile>('/prompt-filter/review/profiles', { method: 'POST', body: JSON.stringify(data) }),
  activatePromptReviewProfile: (id: string) =>
    request<import('./types').PromptReviewProfile>(`/prompt-filter/review/profiles/${encodeURIComponent(id)}/activate`, { method: 'POST' }),
  deletePromptReviewProfile: (id: string) =>
    request<{ ok: boolean }>(`/prompt-filter/review/profiles/${encodeURIComponent(id)}`, { method: 'DELETE' }),
  testPromptFilterRulePattern: (data: { pattern: string; text: string }) =>
    request<PromptFilterRulePatternTestResponse>('/prompt-filter/rules/test', { method: 'POST', body: JSON.stringify(data) }),
  getPromptFilterRules: () =>
    request<PromptFilterRulesResponse>('/prompt-filter/rules'),
  runPromptIntelligence: () =>
    request<import('./types').PromptIntelligenceRun>('/prompt-filter/intelligence/run', { method: 'POST' }),
  getPromptIntelligenceHistory: (page = 1, pageSize = 20) =>
    request<import('./types').PromptIntelligenceHistoryResponse>(`/prompt-filter/intelligence/history?page=${page}&page_size=${pageSize}`),
  getPromptIntelligenceCandidates: (params: { page?: number; pageSize?: number; status?: string; source?: string; q?: string } = {}) => {
    const search = new URLSearchParams()
    search.set('page', String(params.page || 1))
    search.set('page_size', String(params.pageSize || 100))
    if (params.status) search.set('status', params.status)
    if (params.source) search.set('source', params.source)
    if (params.q) search.set('q', params.q)
    return request<import('./types').PromptIntelligenceCandidatesResponse>(`/prompt-filter/intelligence/candidates?${search.toString()}`)
  },
  getPromptIntelligenceCandidateEvidence: (id: number) =>
    request<import('./types').PromptIntelligenceEvidenceResponse>(`/prompt-filter/intelligence/candidates/${id}/evidence`),
  getPromptIntelligenceAIProviders: () =>
    request<import('./types').PromptIntelligenceAIProvidersResponse>('/prompt-filter/intelligence/ai-providers'),
  analyzePromptIntelligenceCandidate: (id: number, data: import('./types').PromptIntelligenceAIAnalysisRequest) =>
    request<import('./types').PromptIntelligenceAIAnalysisResponse>(`/prompt-filter/intelligence/candidates/${id}/analyze`, { method: 'POST', body: JSON.stringify(data) }),
  suggestPromptIntelligenceCandidateDraft: (id: number, data: { provider: import('./types').PromptIntelligenceAIProvider; model?: string; api_key_id?: number }) =>
    request<import('./types').PromptIntelligenceDraftSuggestion>(`/prompt-filter/intelligence/candidates/${id}/draft/suggest`, { method: 'POST', body: JSON.stringify(data), timeoutMs: 90_000 }),
  applyPromptIntelligenceIdentityUpdate: (candidateId: number, evidenceId: number) =>
    request<{ identity_update: import('./types').PromptIdentityUpdateResult }>(`/prompt-filter/intelligence/candidates/${candidateId}/identity-updates/${evidenceId}/apply`, { method: 'POST' }),
  rollbackPromptIntelligenceIdentityUpdate: (candidateId: number, evidenceId: number) =>
    request<{ identity_update: import('./types').PromptIdentityUpdateResult }>(`/prompt-filter/intelligence/candidates/${candidateId}/identity-updates/${evidenceId}/rollback`, { method: 'POST' }),
  createPromptIntelligenceCandidateDraft: (id: number, data: import('./types').PromptIntelligenceRuleDraft) =>
    request<{ candidate: import('./types').PromptIntelligenceCandidate; source_candidate_id: number }>(`/prompt-filter/intelligence/candidates/${id}/draft`, { method: 'POST', body: JSON.stringify(data) }),
  publishPromptIntelligenceCandidate: (id: number) =>
    request<{ candidate: import('./types').PromptIntelligenceCandidate; added: number; updated: number }>(`/prompt-filter/intelligence/candidates/${id}/publish`, { method: 'POST' }),
  dismissPromptIntelligenceCandidate: (id: number) =>
    request<import('./types').PromptIntelligenceCandidate>(`/prompt-filter/intelligence/candidates/${id}/dismiss`, { method: 'POST' }),
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
      models: Array<{
        model: string
        channel?: string
        source: string
        pricing: ModelPricingOverride
        canonical_model?: string
        is_alias?: boolean
      }>
      sync_url: string
      default_sync_url: string
      models_dev_url: string
		official_openai_url: string
		official_xai_url: string
		official_claude_url: string
		official_sync_config: OfficialPricingSyncConfig
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
	updateOfficialPricingSyncConfig: (config: Pick<OfficialPricingSyncConfig, 'enabled' | 'interval_minutes' | 'include_openai' | 'include_grok' | 'include_claude'>) =>
		request<OfficialPricingSyncConfig>('/model-pricing/official-sync/config', {
			method: 'PUT',
			body: JSON.stringify(config),
		}),
	syncOfficialModelPricing: (sources: { include_openai: boolean; include_grok: boolean; include_claude?: boolean }) =>
		request<OfficialPricingSyncResult>('/model-pricing/official-sync', {
			method: 'POST',
			body: JSON.stringify(sources),
			timeoutMs: 95000,
		}),
  batchTestAccounts: (target?: number[] | AccountOperationSelector) =>
    request<{ total: number; success: number; failed: number; banned: number; rate_limited: number }>('/accounts/batch-test', {
      method: 'POST',
      body: target
        ? JSON.stringify(Array.isArray(target) ? { ids: target } : { selector: target })
        : undefined,
    }),
  cleanBanned: () =>
    request<{ message: string; cleaned: number }>('/accounts/clean-banned', { method: 'POST' }),
  cleanRateLimited: () =>
    request<{ message: string; cleaned: number }>('/accounts/clean-rate-limited', { method: 'POST' }),
  cleanError: () =>
    request<{ message: string; cleaned: number }>('/accounts/clean-error', { method: 'POST' }),
  cleanGrokBanned: () =>
    request<{ message: string; cleaned: number }>('/accounts/grok/clean-banned', { method: 'POST' }),
  cleanGrokError: () =>
    request<{ message: string; cleaned: number }>('/accounts/grok/clean-error', { method: 'POST' }),
  cleanAntigravityBanned: () =>
    request<{ message: string; cleaned: number }>('/accounts/antigravity/clean-banned', { method: 'POST' }),
  cleanAntigravityError: () =>
    request<{ message: string; cleaned: number }>('/accounts/antigravity/clean-error', { method: 'POST' }),
  /**
   * 导出账号凭据。includeProxy 打开后条目里会带上账号绑定的代理 URL，
   * 而代理 URL 常含明文用户名密码，因此默认关闭、由调用方显式开启。
   */
  exportAccounts: (params: {
    filter: 'healthy' | 'all'
    ids?: number[]
    channel?: 'codex' | 'grok'
    includeProxy?: boolean
  }) => {
    const sp = new URLSearchParams({ filter: params.filter })
    if (params.ids && params.ids.length > 0) sp.set('ids', params.ids.join(','))
    if (params.channel) sp.set('channel', params.channel)
    if (params.includeProxy) sp.set('include_proxy', '1')
    return request<CPAExportEntry[]>(`/accounts/export?${sp.toString()}`)
  },
  /** 导出回收站账号；ids 为空则导出回收站全部。 */
  exportRecycleBinAccounts: (ids?: number[], includeProxy?: boolean) => {
    const sp = new URLSearchParams()
    if (ids && ids.length > 0) sp.set('ids', ids.join(','))
    if (includeProxy) sp.set('include_proxy', '1')
    const q = sp.toString()
    return request<CPAExportEntry[]>(`/accounts/recycle-bin/export${q ? `?${q}` : ''}`)
  },
  downloadAccountAuthJSON: (id: number) =>
    requestBlob(`/accounts/${id}/auth-json`),
  /**
   * 导出 Grok 账号凭据。ids 为空则导出全部 Grok 账号。
   * 单个账号返回裸 JSON，多个账号返回 ZIP（内部每账号一个 <邮箱>.json）。
   * 文件名由服务端在 Content-Disposition 里给出，前端不再自行拼接。
   */
  exportGrokAccounts: (ids?: number[], includeProxy?: boolean) => {
    const sp = new URLSearchParams({ filter: 'all' })
    if (ids && ids.length > 0) sp.set('ids', ids.join(','))
    if (includeProxy) sp.set('include_proxy', '1')
    return requestNamedBlob(`/accounts/grok/export?${sp.toString()}`)
  },
  /** Admin-only secret-bearing Antigravity credential download (JSON or ZIP). */
  exportAntigravityAccounts: (ids?: number[]) => {
    const sp = new URLSearchParams()
    if (ids && ids.length > 0) sp.set('ids', ids.join(','))
    const query = sp.toString()
    return requestNamedBlob(`/accounts/antigravity/export${query ? `?${query}` : ''}`)
  },
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
  cleanErrorProxies: () =>
    request<{ message: string; cleaned: number; unbound: number }>('/proxies/clean-error', { method: 'POST' }),
  autoBalanceProxies: (data: { channel?: 'codex' | 'grok' | 'claude'; mode?: 'unbound' | 'all'; max_per_proxy?: number; proxy_ids?: number[] }) =>
    request<AutoBalanceProxiesResult>('/proxies/auto-balance', { method: 'POST', body: JSON.stringify(data) }),
  listProxyRiskScoringProfiles: () =>
    request<{ profiles: ProxyRiskScoringProfile[] }>('/proxy-risk-scoring/profiles'),
  createProxyRiskScoringProfile: (data: Partial<ProxyRiskScoringProfile> & { scamalytics_key?: string }) =>
    request<ProxyRiskScoringProfile>('/proxy-risk-scoring/profiles', { method: 'POST', body: JSON.stringify(data) }),
  updateProxyRiskScoringProfile: (id: number, data: Partial<ProxyRiskScoringProfile> & { scamalytics_key?: string }) =>
    request<ProxyRiskScoringProfile>(`/proxy-risk-scoring/profiles/${id}`, { method: 'PATCH', body: JSON.stringify(data) }),
  deleteProxyRiskScoringProfile: (id: number) =>
    request<MessageResponse>(`/proxy-risk-scoring/profiles/${id}`, { method: 'DELETE' }),
  testProxyRiskScoringProfile: (id: number) =>
    request<{ success: boolean; latency_ms?: number; score?: number | null; risk_level?: string; credits_remaining?: number | null; snapshot?: ProxyRiskScoreSnapshot | null; message?: string; error?: string }>(`/proxy-risk-scoring/profiles/${id}/test`, { method: 'POST' }),
  startProxyRiskScoringJob: (data: { profile_id?: number; proxy_ids?: number[]; force?: boolean }) =>
    request<ProxyRiskScoringJob>('/proxies/risk-score', { method: 'POST', body: JSON.stringify(data) }),
  getProxyRiskScoringJob: (id: string, after = 0) =>
    request<ProxyRiskScoringJob>(`/proxies/risk-score/jobs/${encodeURIComponent(id)}${after > 0 ? `?after=${after}` : ''}`),
  cancelProxyRiskScoringJob: (id: string) =>
    request<MessageResponse>(`/proxies/risk-score/jobs/${encodeURIComponent(id)}/cancel`, { method: 'POST' }),
  getProxyRiskScore: (id: number) =>
    request<ProxyRiskScoreSnapshot | { score: null; status: 'unscored' }>(`/proxies/${id}/risk-score`),
  getProxyRiskScoreHistory: (id: number, profileId: number, page = 1, pageSize = 20) =>
    request<{ items: ProxyRiskScoreSnapshot[]; total: number; page: number; page_size: number }>(`/proxies/${id}/risk-score/history?profile_id=${profileId}&page=${page}&page_size=${pageSize}`),
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
  test_status: 'untested' | 'success' | 'error'
  risk_score?: ProxyRiskScoreSnapshot | null
  /** 绑定到该代理的账号数(服务端聚合,前端免拉全量账号)。 */
  bound_count: number
}

export interface AutoBalanceProxiesResult {
  message: string
  assigned: number
  kept: number
  skipped: number
  proxies_used?: number
  distribution?: Array<{ proxy_id: number; label?: string; bound_count: number }>
}

export interface ProxyTestResult {
  success: boolean
  conclusive?: boolean
  ip?: string
  country?: string
  region?: string
  city?: string
  isp?: string
  latency_ms?: number
  location?: string
  error?: string
}
