import type { AccountRow, InviteRecipientRecord } from '../types'

export type InviteRecipientIndex = Record<string, InviteRecipientRecord>

const BLOCKED_INVITE_STATUSES = new Set(['unauthorized', 'error', 'banned'])

export function normalizeInviteEmail(email: string): string {
  return email.trim().toLowerCase()
}

// 邀请收件人状态查询与后端唯一键使用同一口径：trim + lower，并在发请求前去重。
export function normalizeInviteEmails(emails: string[]): string[] {
  const seen = new Set<string>()
  const normalized: string[] = []
  for (const email of emails) {
    const key = normalizeInviteEmail(email)
    if (!key || seen.has(key)) continue
    seen.add(key)
    normalized.push(key)
  }
  return normalized
}

// 已邀请状态只增不减；将服务端结果或刚发送成功的邮箱合并进现有索引。
export function mergeInviteRecipientIndex(
  current: InviteRecipientIndex,
  recipients: InviteRecipientRecord[],
): InviteRecipientIndex {
  if (recipients.length === 0) return current

  let next = current
  for (const recipient of recipients) {
    const key = normalizeInviteEmail(recipient.email)
    if (!key) continue
    if (next === current) next = { ...current }
    next[key] = { ...recipient, email: recipient.email.trim() }
  }
  return next
}

export function inviteRecipientRecord(
  index: InviteRecipientIndex,
  email: string,
): InviteRecipientRecord | undefined {
  return index[normalizeInviteEmail(email)]
}

export function alreadyInvitedEmails(
  emails: string[],
  index: InviteRecipientIndex,
): string[] {
  return emails
    .map((email) => email.trim())
    .filter((email) => Boolean(email) && Boolean(inviteRecipientRecord(index, email)))
}

// 邀请页的两个账号选择器共享这条可见性规则。封禁/错误账号以及被禁用的账号
// 不应再进入候选列表；锁定、限流和其他临时调度状态不影响账号作为受邀方，
// 也不代表其 referral 凭证已经失效，因此继续保留。
export function isInviteAccountSelectable(account: AccountRow): boolean {
  if (account.enabled === false) return false

  const status = (account.status || '').trim().toLowerCase()
  if (BLOCKED_INVITE_STATUSES.has(status)) return false

  return (account.health_tier || '').trim().toLowerCase() !== 'banned'
}

export function isCodexInviteSenderCandidate(account: AccountRow): boolean {
  if (!isInviteAccountSelectable(account)) return false

  // 中转号与 AT-only 账号没有可持续用于 referral 的 Codex OAuth 凭证。
  return !account.openai_responses_api && !account.at_only
}

export function inviteRecipientCandidates(
  rows: AccountRow[],
  excludeEmail?: string,
): AccountRow[] {
  const excluded = (excludeEmail ?? '').trim().toLowerCase()
  const seen = new Set<string>()
  const candidates: AccountRow[] = []

  for (const row of rows) {
    if (!isInviteAccountSelectable(row)) continue

    const email = row.email?.trim()
    if (!email) continue

    const key = email.toLowerCase()
    if (key === excluded || seen.has(key)) continue

    seen.add(key)
    candidates.push(row)
  }

  return candidates
}
