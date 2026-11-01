export const KIB = 1024
export const MIB = 1024 * 1024
export const MIN_OUTPUT_GUARD_TOOL_BYTES = 16 * KIB
export const MAX_OUTPUT_GUARD_TOOL_BYTES = 4 * MIB
export const MIN_OUTPUT_GUARD_TOOL_DELTAS = 256
export const MAX_OUTPUT_GUARD_TOOL_DELTAS = 65536
export const MIN_OUTPUT_GUARD_CHANNEL_BYTES = 128 * KIB
export const MAX_OUTPUT_GUARD_CHANNEL_BYTES = 16 * MIB
export const MIN_OUTPUT_GUARD_REQUEST_BYTES = 256 * KIB
export const MAX_OUTPUT_GUARD_REQUEST_BYTES = 64 * MIB

export interface OutputGuardSettingsValues {
  output_guard_enabled: boolean
  output_guard_max_tool_argument_bytes: number
  output_guard_max_tool_argument_deltas: number
  output_guard_max_channel_bytes: number
  output_guard_max_request_bytes: number
}

export const DEFAULT_OUTPUT_GUARD_SETTINGS: OutputGuardSettingsValues = {
  output_guard_enabled: false,
  output_guard_max_tool_argument_bytes: 256 * KIB,
  output_guard_max_tool_argument_deltas: 4096,
  output_guard_max_channel_bytes: 2 * MIB,
  output_guard_max_request_bytes: 4 * MIB,
}

export const OUTPUT_GUARD_KEYS = [
  'output_guard_enabled',
  'output_guard_max_tool_argument_bytes',
  'output_guard_max_tool_argument_deltas',
  'output_guard_max_channel_bytes',
  'output_guard_max_request_bytes',
] as const

export function isOutputGuardKey(key: string): boolean {
  return OUTPUT_GUARD_KEYS.indexOf(key as (typeof OUTPUT_GUARD_KEYS)[number]) !== -1
}

/**
 * 整表单保存前剔除 guard 写入字段：guard 只允许走自动保存（按 key 打补丁）
 * 路径。整表单提交会把手动打开页面时读到的陈旧快照连同新一轮 generation
 * 一起上送，可能覆盖其他管理员刚改过、甚至刚关闭的 guard 配置。
 */
export function omitOutputGuardFields<T extends object>(settings: T): T {
  const payload = { ...settings }
  for (const key of OUTPUT_GUARD_KEYS) {
    Reflect.deleteProperty(payload, key)
  }
  return payload
}

export function validateOutputGuardHierarchy(settings: {
  output_guard_max_tool_argument_bytes: number
  output_guard_max_channel_bytes: number
  output_guard_max_request_bytes: number
}): string | null {
  if (settings.output_guard_max_tool_argument_bytes > settings.output_guard_max_channel_bytes) {
    return '工具参数上限不能大于单通道上限 (tool <= channel)'
  }
  if (settings.output_guard_max_channel_bytes > settings.output_guard_max_request_bytes) {
    return '单通道上限不能大于单请求上限 (channel <= request)'
  }
  return null
}

export function validateOutputGuardRanges(settings: {
  output_guard_max_tool_argument_bytes?: number
  output_guard_max_tool_argument_deltas?: number
  output_guard_max_channel_bytes?: number
  output_guard_max_request_bytes?: number
}): string | null {
  const tool = settings.output_guard_max_tool_argument_bytes
  if (tool !== undefined && (tool < MIN_OUTPUT_GUARD_TOOL_BYTES || tool > MAX_OUTPUT_GUARD_TOOL_BYTES)) {
    return `工具参数字节范围必须在 ${MIN_OUTPUT_GUARD_TOOL_BYTES} ~ ${MAX_OUTPUT_GUARD_TOOL_BYTES} 之间`
  }
  const deltas = settings.output_guard_max_tool_argument_deltas
  if (deltas !== undefined && (deltas < MIN_OUTPUT_GUARD_TOOL_DELTAS || deltas > MAX_OUTPUT_GUARD_TOOL_DELTAS)) {
    return `工具 Delta 次数必须在 ${MIN_OUTPUT_GUARD_TOOL_DELTAS} ~ ${MAX_OUTPUT_GUARD_TOOL_DELTAS} 之间`
  }
  const channel = settings.output_guard_max_channel_bytes
  if (channel !== undefined && (channel < MIN_OUTPUT_GUARD_CHANNEL_BYTES || channel > MAX_OUTPUT_GUARD_CHANNEL_BYTES)) {
    return `单通道字节范围必须在 ${MIN_OUTPUT_GUARD_CHANNEL_BYTES} ~ ${MAX_OUTPUT_GUARD_CHANNEL_BYTES} 之间`
  }
  const request = settings.output_guard_max_request_bytes
  if (request !== undefined && (request < MIN_OUTPUT_GUARD_REQUEST_BYTES || request > MAX_OUTPUT_GUARD_REQUEST_BYTES)) {
    return `单请求字节范围必须在 ${MIN_OUTPUT_GUARD_REQUEST_BYTES} ~ ${MAX_OUTPUT_GUARD_REQUEST_BYTES} 之间`
  }
  return null
}

/**
 * 从响应或表单中仅保留非 guard 字段（用 current 中的 guard 字段替换或从响应中剔除 guard 字段）。
 *
 * 手动保存时不应让服务器响应覆盖当前表单和基线中的 guard 字段（guard 字段仅走自动保存）。
 */
export function preserveOutputGuardFields<T extends object>(
  updated: T,
  current: T,
): T {
  const result = { ...updated }
  for (const key of OUTPUT_GUARD_KEYS) {
    if (Object.prototype.hasOwnProperty.call(current, key)) {
      result[key as keyof T] = current[key as keyof T]
    }
  }
  if (Object.prototype.hasOwnProperty.call(current, 'output_guard_config_generation')) {
    (result as Record<string, unknown>).output_guard_config_generation =
      (current as Record<string, unknown>).output_guard_config_generation
  }
  return result
}

export function bytesToKiB(bytes: number): number { return Math.round(bytes / KIB) }
export function kibToBytes(kib: number): number { return kib * KIB }
export function bytesToMiB(bytes: number): number { return Math.round(bytes / MIB) }
export function mibToBytes(mib: number): number { return mib * MIB }
