import assert from 'node:assert/strict'
import { readFileSync } from 'node:fs'
import test from 'node:test'

const source = readFileSync(new URL('../pages/PromptFilter.tsx', import.meta.url), 'utf8')
const types = readFileSync(new URL('../types.ts', import.meta.url), 'utf8')
const api = readFileSync(new URL('../api.ts', import.meta.url), 'utf8')

test('CY incidents and local logs use independent pagination state', () => {
  assert.match(source, /usePersistedPageSize\('prompt_policy_incidents'/)
  assert.match(source, /page: incidentPage,\s+pageSize: incidentPageSize/)
  assert.match(source, /page: pageOverride \?\? logPage,/)
  assert.match(source, /page=\{incidentPage\}[\s\S]*totalItems=\{incidentTotal\}/)
  assert.match(source, /page=\{logPage\}[\s\S]*totalItems=\{total\}/)
  assert.doesNotMatch(source, /Math\.max\(total, incidentTotal\)/)
})

test('CY routing snapshots and NewAPI audit passthrough are visible', () => {
  for (const field of [
    'account_name',
    'account_group_names',
    'api_key_allowed_group_names',
	'routing_snapshot_state',
    'local_comparison',
    'prompt_available',
    'newapi_policy_status',
    'newapi_request_id',
    'newapi_decision_id',
  ]) {
    assert.match(types, new RegExp(`${field}[?:]`))
  }
  assert.match(source, /cyberComparisonStatus/)
	assert.match(source, /cyberRoutingState/)
  assert.match(source, /newapiPolicyStatus/)
})

test('Prompt log tables show the complete API key name inside the fixed-width column', () => {
  assert.match(source, /const apiKeyLabel = log\.api_key_name \|\| log\.api_key_masked \|\| '-'/)
  assert.match(source, /className="whitespace-normal break-all font-mono text-\[11px\] leading-4 text-foreground" title=\{apiKeyLabel\}/)
  assert.doesNotMatch(source, /max-w-\[110px\] truncate[^\n]*api_key_name/)
})

test('CY audit chain has a read-only health check without generating incidents', () => {
  assert.match(api, /getPromptPolicyAuditHealth/)
  assert.match(api, /\/prompt-policy\/incidents\/health/)
  assert.match(types, /export interface PromptPolicyAuditHealth/)
  assert.match(source, /showAuditHealth/)
  assert.match(source, /auditHealth\.queue\.failed \+ auditHealth\.queue\.dropped_high/)
  assert.match(source, /auditHealth\.review_pool\.available/)
  assert.match(source, /auditHealth\.review_fail_closed/)
  assert.equal(typeof JSON.parse(readFileSync(new URL('../locales/zh.json', import.meta.url), 'utf8')).promptFilter.auditHealth.fallbackLocal, 'string')
  assert.doesNotMatch(source, /createSyntheticPromptPolicyIncident/)
})
