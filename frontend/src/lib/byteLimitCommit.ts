/**
 * 字节上限字段在表单里按 KiB/MiB 展示，展示值是四舍五入后的整数。blur 时若把
 * 展示值原样换算回字节写给服务器，持久化的非整倍数字节值（如 131073）会被
 * 无声改成 131072。
 *
 * 这里以最后一次持久化的原始字节值为基准：展示单位值没变就跳过提交并保留原
 * 字节值，只有用户实际修改了单位值才提交。基准必须是持久化值——表单值在键入
 * 时就已经被改成本次换算后的字节值，拿表单值比较会把未编辑的 blur 也判成
 * 编辑，从而漏掉这条保护。
 */

export interface ByteLimitCommitDecision {
  /** 是否应把 bytes 提交给服务器；false 表示单位值未变，保留持久化原值。 */
  submit: boolean
  /** submit=true 时是本次提交的字节值；false 时是持久化的原始字节值。 */
  bytes: number
}

export function resolveByteLimitCommit(
  persistedBytes: number | null | undefined,
  submittedUnits: number,
  bytesPerUnit: number,
): ByteLimitCommitDecision {
  const submittedBytes = submittedUnits * bytesPerUnit
  if (typeof persistedBytes !== 'number' || !Number.isFinite(persistedBytes)) {
    return { submit: true, bytes: submittedBytes }
  }
  if (Math.round(persistedBytes / bytesPerUnit) === submittedUnits) {
    return { submit: false, bytes: persistedBytes }
  }
  return { submit: true, bytes: submittedBytes }
}
