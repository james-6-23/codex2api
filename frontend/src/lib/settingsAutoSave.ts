/**
 * 设置页自动保存的"服务器确认值"辅助函数。
 *
 * 键入型字段（如 output guard 的 channel limit）在 blur 提交前就会把草稿同步
 * 到 settingsFormRef，所以自动保存失败时，"提交前的表单值"可能只是用户刚敲
 * 进去、从未被服务器确认过的取值。回滚必须回到最后一次服务器确认的快照，
 * 而不是回到这份草稿。
 */

/** 自动保存失败时用于回滚的字段值：优先取最后的服务器确认快照，快照尚未
 * 建立（首次加载完成前）时才退回提交前的表单值。 */
export function resolveAutoSaveRollbackValues<T extends object>(
  confirmed: T | null,
  submitted: T,
  keys: ReadonlyArray<keyof T>,
): Partial<T> {
  const source = confirmed ?? submitted
  const values: Partial<T> = {}
  for (const key of keys) {
    values[key] = source[key]
  }
  return values
}

/** 把一次成功请求返回的服务器确认值合并进快照，只覆盖该请求提交的字段，
 * 不整份覆盖其他尚未确认或由其他请求负责的字段。 */
export function mergeConfirmedSettingsValues<T extends object>(
  confirmed: T | null,
  confirmedValues: Partial<T>,
): T | null {
  return confirmed ? { ...confirmed, ...confirmedValues } : confirmed
}

export interface ConfirmedSettingsVersionState<T extends object> {
  confirmed: T | null
  confirmedVersions: Record<string, number>
}

/**
 * 带有字段版本检查的服务器确认值合并函数。
 *
 * 为每个字段记录"最后确认的请求版本"；合并时逐字段比较本次响应所携带的请求版本
 * 是否新于该字段最后确认版本。若旧响应晚于新响应到达，跳过旧字段值，防止回滚基线
 * 被冲回过期值。
 */
export function mergeConfirmedSettingsWithVersions<T extends object>(
  confirmed: T | null,
  confirmedVersions: Record<string, number>,
  incomingValues: Partial<T>,
  incomingVersions: Record<string, number>,
): ConfirmedSettingsVersionState<T> {
  const nextVersions: Record<string, number> = { ...confirmedVersions }
  const acceptableValues: Partial<T> = {}
  let hasUpdate = false

  for (const key of Object.keys(incomingValues) as Array<keyof T>) {
    const fieldKey = String(key)
    const incomingVer = incomingVersions[fieldKey] ?? 0
    const currentVer = nextVersions[fieldKey] ?? 0
    if (incomingVer > currentVer) {
      acceptableValues[key] = incomingValues[key]
      nextVersions[fieldKey] = incomingVer
      hasUpdate = true
    }
  }

  if (!confirmed) {
    return {
      confirmed: null,
      confirmedVersions: nextVersions,
    }
  }

  if (!hasUpdate) {
    return {
      confirmed,
      confirmedVersions: nextVersions,
    }
  }

  return {
    confirmed: { ...confirmed, ...acceptableValues },
    confirmedVersions: nextVersions,
  }
}

