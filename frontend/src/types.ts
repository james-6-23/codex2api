export type ToastType = 'success' | 'error'
export type ISODateString = string

export interface ToastState {
  msg: string
  type: ToastType
}

export type AccountStatus = 'active' | 'ready' | 'cooldown' | 'error' | 'refreshing' | 'paused' | string

export interface StatsResponse {
  total: number
  available: number
  error: number
  today_requests: number
}

export interface AccountUsageWindow {
  requests: number
  tokens: number
  account_billed?: number
  user_billed?: number
}

export interface AccountRow {
  id: number
  name: string
  email: string
  plan_type: string
  status: AccountStatus
  error_message?: string
  at_only?: boolean
  account_type?: string
  openai_responses_api?: boolean
  base_url?: string
  models?: string[]
  health_tier?: string
  scheduler_score?: number
  dispatch_score?: number
  score_bias_override?: number | null
  score_bias_effective?: number
  base_concurrency_override?: number | null
  base_concurrency_effective?: number
  dynamic_concurrency_limit?: number
  allowed_api_key_ids?: number[]
  tags?: string[]
  group_ids?: number[]
  scheduler_breakdown?: {
    unauthorized_penalty: number
    rate_limit_penalty: number
    timeout_penalty: number
    server_penalty: number
    failure_penalty: number
    success_bonus: number
    usage_penalty_7d: number
    usage_urgency_bonus_5h?: number
    usage_urgency_bonus_7d?: number
    latency_penalty: number
    success_rate_penalty?: number
  }
  last_unauthorized_at?: ISODateString
  last_rate_limited_at?: ISODateString
  last_timeout_at?: ISODateString
  last_server_error_at?: ISODateString
  proxy_url: string
  created_at: ISODateString
  updated_at: ISODateString
  codex_usage_updated_at?: ISODateString
  active_requests?: number
  total_requests?: number
  last_used_at?: ISODateString
  success_requests?: number
  error_requests?: number
  retry_error_requests?: number
  rate_limit_attempts?: number
  usage_percent_7d?: number | null
  usage_percent_5h?: number | null
  usage_5h_detail?: AccountUsageWindow
  usage_7d_detail?: AccountUsageWindow
  reset_5h_at?: ISODateString
  reset_7d_at?: ISODateString
  billed_5h?: number
  billed_7d?: number
  cooldown_until?: ISODateString
  cooldown_reason?: string
  model_cooldowns?: Array<{
    model: string
    reason: string
    reset_at: ISODateString
    remaining_seconds: number
  }>
  enabled?: boolean
  locked?: boolean
  credit_enabled?: boolean
  credit_skip_usage_window?: boolean
  // 图片配额信息
  image_quota_remaining?: number
  image_quota_total?: number
  today_used_count?: number
  image_quota_reset_at?: ISODateString
}

export type AccountsResponse = ApiListResponse<'accounts', AccountRow>

export interface AddAccountRequest {
  name?: string
  refresh_token?: string
  session_token?: string
  proxy_url: string
}

export interface AddATAccountRequest {
  name?: string
  access_token: string
  proxy_url: string
}

export interface AddOpenAIResponsesAccountRequest {
  name?: string
  base_url: string
  api_key: string
  models: string[]
  proxy_url: string
}

export interface UpdateOpenAIResponsesAccountRequest {
  name?: string
  base_url: string
  api_key?: string
  models: string[]
  proxy_url: string
}

export interface FetchOpenAIResponsesModelsRequest {
  account_id?: number
  base_url: string
  api_key: string
  proxy_url?: string
}

export interface FetchOpenAIResponsesModelsResponse {
  base_url: string
  models: string[]
}

export interface UpdateAccountSchedulerRequest {
  score_bias_override?: number | null
  base_concurrency_override?: number | null
  allowed_api_key_ids?: number[] | null
  proxy_url?: string | null
  tags?: string[] | null
  group_ids?: number[] | null
}

export interface AccountGroup {
  id: number
  name: string
  description: string
  color: string
  sort_order: number
  member_count: number
  created_at: ISODateString
  updated_at: ISODateString
}

export interface AccountGroupsResponse {
  groups: AccountGroup[]
}

export interface CreateAccountGroupRequest {
  name: string
  description?: string
  color?: string
  sort_order?: number
}

export interface UpdateAccountGroupRequest {
  name?: string
  description?: string
  color?: string
  sort_order?: number
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
  cache_hit_rate: number
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

export interface SiteBranding {
  site_name: string
  site_logo: string
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
    process_bytes: number
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
    avg_duration_ms: number
  }
}

export interface SystemSettings {
  site_name: string
  site_logo: string
  max_concurrency: number
  global_rpm: number
  test_model: string
  test_concurrency: number
  background_refresh_interval_minutes: number
  usage_probe_max_age_minutes: number
  recovery_probe_interval_minutes: number
  lazy_mode: boolean
  proxy_url?: string
  pg_max_conns: number
  redis_pool_size: number
  auto_clean_unauthorized: boolean
  auto_clean_rate_limited: boolean
  admin_secret: string
  admin_secret_configured?: boolean
  admin_auth_source: 'env' | 'database' | 'disabled' | string
  auto_clean_full_usage: boolean
  auto_clean_error: boolean
  auto_clean_expired: boolean
  proxy_pool_enabled: boolean
  fast_scheduler_enabled: boolean
  scheduler_mode: string
  max_retries: number
  max_rate_limit_retries: number
  allow_remote_migration: boolean
  database_driver: string
  database_label: string
  cache_driver: string
  cache_label: string
  expired_cleaned?: number
  model_mapping: string
  resin_url: string
  resin_platform_name: string
  prompt_filter_enabled: boolean
  prompt_filter_mode: 'monitor' | 'warn' | 'block' | string
  prompt_filter_threshold: number
  prompt_filter_strict_threshold: number
  prompt_filter_log_matches: boolean
  prompt_filter_max_text_length: number
  prompt_filter_sensitive_words: string
  prompt_filter_custom_patterns: string
  prompt_filter_disabled_patterns: string
  client_compat_mode: 'preserve' | 'auto' | 'force' | string
  codex_min_cli_version: string
  usage_log_mode: 'full' | 'errors' | 'off' | string
  usage_log_batch_size: number
  usage_log_flush_interval_seconds: number
  stream_flush_policy: 'immediate' | 'coalesce' | string
  stream_flush_interval_ms: number
  image_storage_backend: 'local' | 's3' | string
  image_s3_endpoint: string
  image_s3_region: string
  image_s3_bucket: string
  image_s3_access_key: string
  image_s3_secret_key: string
  image_s3_secret_key_configured?: boolean
  image_s3_prefix: string
  image_s3_force_path_style: boolean
}

export interface PromptFilterMatch {
  name: string
  weight: number
  category: string
  strict: boolean
}

export interface PromptFilterVerdict {
  enabled: boolean
  mode: string
  action: 'allow' | 'warn' | 'block' | string
  score: number
  raw_score: number
  threshold: number
  strict_hit: boolean
  matched: PromptFilterMatch[]
  text_preview: string
  reason: string
  extracted_chars: number
}

export interface PromptFilterLog {
  id: number
  created_at: ISODateString
  source: string
  endpoint: string
  model: string
  action: string
  mode: string
  score: number
  threshold: number
  matched_patterns: string
  text_preview: string
  api_key_id: number
  api_key_name: string
  api_key_masked: string
  client_ip: string
  error_code: string
}

export interface PromptFilterLogsResponse {
  logs: PromptFilterLog[]
  total: number
  page: number
  page_size: number
}

export interface PromptFilterTestResponse {
  verdict: PromptFilterVerdict
}

export interface PromptFilterRule {
  name: string
  pattern: string
  weight: number
  category?: string
  strict?: boolean
  enabled?: boolean
  builtin?: boolean
}

export interface PromptFilterRulesResponse {
  builtin_patterns: PromptFilterRule[]
  custom_patterns: PromptFilterRule[]
  disabled_patterns: string[]
}

export interface ModelInfo {
  id: string
  enabled: boolean
  category: string
  source: string
  pro_only: boolean
  api_key_auth_available: boolean
  last_seen_at?: string
  updated_at?: string
}

export interface ModelsResponse {
  models: string[]
  items?: ModelInfo[]
  last_synced_at?: string
  source_url: string
  warning?: string
}

export interface ModelSyncResponse {
  added: number
  updated: number
  unchanged: number
  skipped: string[]
  models: string[]
  items: ModelInfo[]
  last_synced_at: string
  source_url: string
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
  total_input_tokens?: number
  total_cached_tokens: number
  total_cache_rate?: number
  total_account_billed: number
  total_user_billed: number
  avg_account_billed_per_request: number
  avg_user_billed_per_request: number
  today_requests: number
  today_tokens: number
  today_input_tokens?: number
  today_cached_tokens?: number
  today_cache_rate?: number
  today_account_billed: number
  today_user_billed: number
  rpm: number
  tpm: number
  avg_duration_ms: number
  avg_first_token_ms?: number
  error_rate: number
  feature_stats: UsageFeatureStats
  model_stats: UsageModelStat[]
  endpoint_stats: UsageEndpointStat[]
  api_key_stats: UsageAPIKeyStat[]
}

export interface UsageModelStat {
  model: string
  requests: number
  tokens: number
  input_tokens: number
  output_tokens: number
  cached_tokens: number
  account_billed: number
  user_billed: number
  error_count: number
}

export interface UsageFeatureStats {
  stream_requests: number
  sync_requests: number
  fast_requests: number
  cache_hit_requests: number
  reasoning_requests: number
  image_requests: number
  retry_requests: number
  error_requests: number
}

export interface UsageEndpointStat {
  endpoint: string
  requests: number
  tokens: number
  error_count: number
  user_billed: number
}

export interface UsageAPIKeyStat {
  api_key_id: number
  label: string
  requests: number
  tokens: number
  error_count: number
  user_billed: number
}

export interface UsageLog {
  id: number
  account_id: number
  endpoint: string
  model: string
  effective_model: string
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
  api_key_id: number
  api_key_name: string
  api_key_masked: string
  image_count: number
  image_width: number
  image_height: number
  image_bytes: number
  image_format: string
  image_size: string
  account_email: string
  created_at: ISODateString
  account_billed: number
  user_billed: number
  input_cost: number
  output_cost: number
  cache_read_cost: number
  total_cost: number
  input_price_per_mtoken: number
  output_price_per_mtoken: number
  cache_read_price_per_mtoken: number
  rate_multiplier: number
  is_retry_attempt: boolean
  attempt_index: number
  upstream_error_kind: string
  error_message: string
}

export type UsageLogsResponse = ApiListResponse<'logs', UsageLog>

export interface UsageLogsPagedResponse {
  logs: UsageLog[]
  total: number
}

export interface OpsErrorSummary {
  total_errors: number
  status_4xx: number
  status_5xx: number
  unauthorized: number
  rate_limited: number
  canceled: number
  timeouts: number
  retry_attempts: number
  avg_duration_ms: number
}

export interface ChartTimelinePoint {
  bucket: string
  requests: number
  avg_latency: number
  input_tokens: number
  output_tokens: number
  reasoning_tokens: number
  cached_tokens: number
  errors_4xx: number
  errors_5xx: number
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
  raw_key: string
  quota_limit: number
  quota_used: number
  expires_at?: ISODateString | null
  status?: 'active' | 'expired' | 'quota_exhausted'
  allowed_group_ids?: number[]
  created_at: ISODateString
}

export type APIKeysResponse = ApiListResponse<'keys', APIKeyRow>

export interface CreateAPIKeyRequest {
  name: string
  key?: string
  quota_limit?: number
  quota?: number
  expires_at?: string
  expires_in_days?: number
  allowed_group_ids?: number[]
}

export interface UpdateAPIKeyRequest {
  name?: string
  quota_limit?: number | null
  quota?: number | null
  expires_at?: string | null
  expires_in_days?: number
  allowed_group_ids?: number[]
}

export interface CreateAPIKeyResponse {
  id: number
  key: string
  name: string
  quota_limit: number
  quota_used: number
  expires_at?: ISODateString | null
  allowed_group_ids?: number[]
}

export interface ImagePromptTemplate {
  id: number
  name: string
  prompt: string
  model: string
  size: string
  quality: string
  output_format: string
  background: string
  style: string
  tags: string[]
  favorite: boolean
  usage_count: number
  last_used_at?: ISODateString
  created_at: ISODateString
  updated_at: ISODateString
}

export interface ImageAsset {
  id: number
  job_id: number
  template_id: number
  filename: string
  proxy_url?: string
  thumbnail_url?: string
  mime_type: string
  bytes: number
  width: number
  height: number
  model: string
  requested_size: string
  actual_size: string
  quality: string
  output_format: string
  revised_prompt: string
  created_at: ISODateString
  cache_b64_json?: string
}

export interface ImageGenerationJob {
  id: number
  status: 'queued' | 'running' | 'succeeded' | 'failed' | string
  prompt: string
  params_json: string
  api_key_id: number
  api_key_name: string
  api_key_masked: string
  error_message: string
  duration_ms: number
  created_at: ISODateString
  started_at?: ISODateString
  completed_at?: ISODateString
  assets?: ImageAsset[]
}

export interface ImagePromptTemplatesResponse {
  templates: ImagePromptTemplate[]
}

export interface ImageJobResponse {
  job: ImageGenerationJob
}

export interface ImageJobsResponse {
  jobs: ImageGenerationJob[]
  total: number
}

export interface ImageAssetsResponse {
  assets: ImageAsset[]
  total: number
}

export interface ImagePromptTemplatePayload {
  name?: string
  prompt?: string
  model?: string
  size?: string
  quality?: string
  output_format?: string
  background?: string
  style?: string
  tags?: string[]
  favorite?: boolean
}

export interface CreateImageJobPayload {
  prompt: string
  model?: string
  size?: string
  quality?: string
  output_format?: string
  background?: string
  style?: string
  upscale?: string
  api_key_id?: number
  template_id?: number
  input_images?: string[]
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
