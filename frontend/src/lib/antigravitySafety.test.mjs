import assert from 'node:assert/strict'
import { readFileSync } from 'node:fs'
import test from 'node:test'
import { extractBalancedBody } from './sourceBoundary.mjs'

const accountsSource = readFileSync(
  new URL('../pages/AntigravityAccounts.tsx', import.meta.url),
  'utf8',
)
const dashboardSource = readFileSync(
  new URL('../pages/Dashboard.tsx', import.meta.url),
  'utf8',
)
const apiKeysSource = readFileSync(
  new URL('../pages/APIKeys.tsx', import.meta.url),
  'utf8',
)
const apiSource = readFileSync(new URL('../api.ts', import.meta.url), 'utf8')
const typesSource = readFileSync(new URL('../types.ts', import.meta.url), 'utf8')

test('API-key editing hydrates lite rows before initializing secret-bearing settings', () => {
  const openEditor = extractBalancedBody(accountsSource, 'const openEditor = async')
  for (const fragment of [
    'account.detail_loaded !== false',
    'setEditingDetailsLoading(true)',
    'await api.getAccount(account.id)',
    'editLoadGenerationRef.current !== generation',
    'setEditDraft(editDraftFromAccount(detail))',
  ]) {
    assert.equal(openEditor.includes(fragment), true, `missing safe edit hydration: ${fragment}`)
  }
  assert.equal(accountsSource.includes('editing || editingDetailsLoading || Boolean(editingDetailsError)'), true)
})

test('detail management requests are account-and-generation scoped', () => {
  const openDetail = extractBalancedBody(accountsSource, 'const openDetailAccount')
  for (const fragment of [
    'detailGenerationRef.current += 1',
    'detailAccountIdRef.current = accountId',
    'setManagementState(null)',
    'setManagementError(null)',
    'setManagementAction(null)',
  ]) {
    assert.equal(openDetail.includes(fragment), true, `detail switch does not reset: ${fragment}`)
  }

  for (const marker of ['const handleStateSync', 'const handleCapabilityProbe']) {
    const action = extractBalancedBody(accountsSource, marker)
    assert.equal(action.includes('const accountId = detailAccountIdRef.current'), true)
    assert.equal(action.includes('const generation = detailGenerationRef.current'), true)
    assert.equal(action.includes('isCurrentDetailRequest(accountId, generation)'), true)
  }
  assert.equal(accountsSource.includes('getAntigravityAccountState(accountId, controller.signal)'), true)
  assert.ok(
    (accountsSource.match(/isCurrentDetailRequest\(accountId, generation\)/g) ?? []).length >= 8,
    'state load, sync, probe, errors, and finalizers must all reject stale generations',
  )
})

test('batch import identity includes sub-index in both React key and visible source label', () => {
  assert.equal(typesSource.includes('sub_index?: number'), true)
  assert.equal(accountsSource.includes('item.sub_index ?? 0'), true)
  assert.equal(accountsSource.includes('`#${item.index}${suffix}`'), true)
  assert.equal(accountsSource.includes('importItemDisplayLabel(item)'), true)
})

test('dashboard pool analysis follows the selected channel and rejects stale responses', () => {
  const loadPool = extractBalancedBody(dashboardSource, 'const loadPoolRunwayData')
  for (const fragment of [
    'const requestedChannel = channel',
    'api.getAccountAnalysis(requestedChannel, controller.signal)',
    'generation !== poolRequestGenerationRef.current',
    'channelRef.current !== requestedChannel',
  ]) {
    assert.equal(loadPool.includes(fragment), true, `pool request missing guard: ${fragment}`)
  }
  assert.equal(dashboardSource.includes("getAccountAnalysis('codex')"), false)

  const switchChannel = extractBalancedBody(dashboardSource, 'const handleChannelChange')
  for (const fragment of [
    'poolAbort.current?.abort()',
    'poolRequestGenerationRef.current += 1',
    'poolDataRef.current = null',
    'accountAnalysis: null',
  ]) {
    assert.equal(switchChannel.includes(fragment), true, `channel switch leaks pool state: ${fragment}`)
  }
  assert.equal(dashboardSource.includes('if (!showPoolRunway || !channel) return'), true)
  assert.equal(dashboardSource.includes("t('dashboard.poolRunwaySingleChannelOnly')"), true)
})

test('API-key group selectors are channel filtered and channel changes prune both policies', () => {
  const groupFilter = extractBalancedBody(apiKeysSource, 'function accountGroupsForUpstreamChannel')
  assert.equal(groupFilter.includes('channel === "auto"'), true)
  assert.equal(groupFilter.includes('group.channel === channel'), true)

  for (const marker of ['const updateCreateUpstreamChannel', 'const updateEditUpstreamChannel']) {
    const update = extractBalancedBody(apiKeysSource, marker)
    assert.equal(update.includes('allowedGroupIds: compatibleGroupIdsForUpstreamChannel'), true)
    assert.equal(update.includes('noAffinityGroupIds: compatibleGroupIdsForUpstreamChannel'), true)
  }
  assert.equal(apiKeysSource.includes('groups={createSelectableGroups}'), true)
  assert.ok(
    (apiKeysSource.match(/groups=\{editSelectableGroups\}/g) ?? []).length >= 2,
    'allowed and no-affinity selectors must share the filtered edit group list',
  )
})

test('full credential export confirms scope and trusts only the response count', () => {
  const handleExport = extractBalancedBody(accountsSource, 'const handleExport')
  for (const fragment of [
    'exportAllConfirmTitle',
    'filtersActive',
    'exportAllConfirmFiltered',
    'count: responseCount',
    'responseCount ?? (ids?.length || undefined)',
    'exportSuccessUnknownCount',
  ]) {
    assert.equal(handleExport.includes(fragment), true, `export safety missing: ${fragment}`)
  }
  assert.equal(handleExport.includes('totalAccounts'), false)
  assert.equal(apiSource.includes('count?: number'), true)
  assert.equal(apiSource.includes("res.headers.get('X-Export-Count')"), true)
})
