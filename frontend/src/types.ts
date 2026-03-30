export type ToastType = 'success' | 'error'
export type ISODateString = string

export interface ToastState {
  msg: string
  type: ToastType
}

export type AccountStatus = 'active' | 'ready' | 'cooldown' | 'error' | 'paused' | string

export interface StatsResponse {
  total: number
  available: number
  error: number
  today_requests: number
}

export interface AccountRow {
  id: number
  name: string
  email: string
  plan_type: string
  status: AccountStatus
  at_only?: boolean
  health_tier?: string
  scheduler_score?: number
  dynamic_concurrency_limit?: number
  scheduler_breakdown?: {
    unauthorized_penalty: number
    rate_limit_penalty: number
    timeout_penalty: number
    server_penalty: number
    failure_penalty: number
    success_bonus: number
    usage_penalty_7d: number
    latency_penalty: number
  }
  last_unauthorized_at?: ISODateString
  last_rate_limited_at?: ISODateString
  last_timeout_at?: ISODateString
  last_server_error_at?: ISODateString
  proxy_url: string
  created_at: ISODateString
  updated_at: ISODateString
  active_requests?: number
  total_requests?: number
  last_used_at?: ISODateString
  success_requests?: number
  error_requests?: number
  usage_percent_7d?: number | null
  usage_percent_5h?: number | null
  reset_5h_at?: ISODateString
  reset_7d_at?: ISODateString
  locked?: number
}

export type AccountsResponse = ApiListResponse<'accounts', AccountRow>

export interface AddAccountRequest {
  name?: string
  refresh_token: string
  proxy_url: string
}

export interface AddATAccountRequest {
  name?: string
  access_token: string
  proxy_url: string
}

export interface AccountModelStat {
  model: string
  requests: number
  tokens: number
}

export interface AccountUsageDetail {
  total_requests: number
  total_tokens: number
  input_tokens: number
  output_tokens: number
  reasoning_tokens: number
  cached_tokens: number
  models: AccountModelStat[]
}

export interface MessageResponse {
  message: string
}

export interface CreateAccountResponse extends MessageResponse {
  id: number
}

export interface AdminErrorResponse {
  error: string
}

export interface HealthResponse {
  status: 'ok' | string
  available: number
  total: number
}

export interface AccountEventTrendPoint {
  bucket: string
  added: number
  deleted: number
}

export interface OpsOverviewResponse {
  updated_at: ISODateString
  uptime_seconds: number
  database_driver: string
  database_label: string
  cache_driver: string
  cache_label: string
  cpu: {
    percent: number
    cores: number
  }
  memory: {
    percent: number
    used_bytes: number
    total_bytes: number
  }
  runtime: {
    goroutines: number
    available_accounts: number
    total_accounts: number
  }
  requests: {
    active: number
    total: number
  }
  postgres: {
    healthy: boolean
    open: number
    in_use: number
    idle: number
    max_open: number
    wait_count: number
    usage_percent: number
  }
  redis: {
    healthy: boolean
    total_conns: number
    idle_conns: number
    stale_conns: number
    pool_size: number
    usage_percent: number
  }
  traffic: {
    qps: number
    qps_peak: number
    tps: number
    tps_peak: number
    rpm: number
    tpm: number
    error_rate: number
    today_requests: number
    today_tokens: number
    rpm_limit: number
  }
}

export interface SystemSettings {
  max_concurrency: number
  global_rpm: number
  test_model: string
  test_concurrency: number
  proxy_url?: string
  pg_max_conns: number
  redis_pool_size: number
  auto_clean_unauthorized: boolean
  auto_clean_rate_limited: boolean
  admin_secret: string
  admin_auth_source: 'env' | 'database' | 'disabled' | string
  auto_clean_full_usage: boolean
  auto_clean_error: boolean
  auto_clean_expired: boolean
  proxy_pool_enabled: boolean
  fast_scheduler_enabled: boolean
  max_retries: number
  allow_remote_migration: boolean
  database_driver: string
  database_label: string
  cache_driver: string
  cache_label: string
  expired_cleaned?: number
}

export interface CPAExportEntry {
  type: string
  email: string
  expired: string
  id_token: string
  account_id: string
  access_token: string
  last_refresh: string
  refresh_token: string
}

export interface UsageStats {
  total_requests: number
  total_tokens: number
  total_prompt_tokens: number
  total_completion_tokens: number
  total_cached_tokens: number
  today_requests: number
  today_tokens: number
  rpm: number
  tpm: number
  avg_duration_ms: number
  error_rate: number
}

export interface UsageLog {
  id: number
  account_id: number
  endpoint: string
  model: string
  prompt_tokens: number
  completion_tokens: number
  total_tokens: number
  status_code: number
  duration_ms: number
  input_tokens: number
  output_tokens: number
  reasoning_tokens: number
  first_token_ms: number
  reasoning_effort: string
  inbound_endpoint: string
  upstream_endpoint: string
  stream: boolean
  cached_tokens: number
  service_tier: string
  account_email: string
  created_at: ISODateString
}

export type UsageLogsResponse = ApiListResponse<'logs', UsageLog>

export interface UsageLogsPagedResponse {
  logs: UsageLog[]
  total: number
}

export interface ChartTimelinePoint {
  bucket: string
  requests: number
  avg_latency: number
  input_tokens: number
  output_tokens: number
  reasoning_tokens: number
  cached_tokens: number
  errors_401: number
}

export interface ChartModelPoint {
  model: string
  requests: number
}

export interface ChartAggregation {
  timeline: ChartTimelinePoint[]
  models: ChartModelPoint[]
}

export interface APIKeyRow {
  id: number
  name: string
  key: string
  created_at: ISODateString
}

export type APIKeysResponse = ApiListResponse<'keys', APIKeyRow>

export interface CreateAPIKeyResponse {
  id: number
  key: string
  name: string
}

export type ApiListResponse<K extends string, T> = {
  [P in K]: T[]
}

export interface OAuthURLResponse {
  auth_url: string
  session_id: string
}

export interface OAuthExchangeResponse {
  message: string
  id: number
  email: string
  plan_type: string
}
