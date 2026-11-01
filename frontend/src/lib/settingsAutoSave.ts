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
