import assert from 'node:assert/strict'
import { readFileSync } from 'node:fs'
import test from 'node:test'

const proxies = readFileSync(new URL('../pages/Proxies.tsx', import.meta.url), 'utf8')
const api = readFileSync(new URL('../api.ts', import.meta.url), 'utf8')
const docs = readFileSync(new URL('../../../docs/proxy-risk-scoring.md', import.meta.url), 'utf8')

test('proxy list keeps all risk score detail categories visible', () => {
  for (const token of ['riskScoreValueColumn', 'riskLevelColumn', 'riskFeaturesColumn', 'riskISPColumn', 'riskRecommendationColumn', 'risk_level', 'proxy_type', 'is_blacklisted', 'isp', 'recommendation', 'checked_at']) {
    assert.match(proxies, new RegExp(token))
  }
  assert.match(proxies, /riskReferenceOnly/)
  assert.match(proxies, /riskScoreCurrentPage/)
  assert.match(proxies, /riskScoreAll/)
  assert.match(proxies, /riskBuiltInEngine/)
  assert.match(proxies, /table-fixed/)
  assert.match(proxies, /riskTestResult/)
  assert.match(proxies, /new Set\(features\)/)
  assert.match(proxies, /w-\[180px\].*riskRecommendationColumn/)
  assert.match(proxies, /w-\[270px\] min-w-\[270px\]/)
  assert.match(proxies, /colActions/)
  assert.match(proxies, /min-w-\[2[0-9]{3}px\]/)
  assert.match(proxies, /<colgroup>/)
  assert.match(proxies, /break-words/)
  assert.match(proxies, /score\.isp/)
  assert.doesNotMatch(proxies, /riskProfileBaseURL/)
  assert.doesNotMatch(proxies, /riskAccessToken/)
})

test('proxy scoring API exposes profile, async job, latest and history operations', () => {
  for (const token of [
    'listProxyRiskScoringProfiles',
    'createProxyRiskScoringProfile',
    'testProxyRiskScoringProfile',
    'startProxyRiskScoringJob',
    'getProxyRiskScoringJob',
    'getProxyRiskScoreHistory',
    'deleteProxyRiskScoringProfile',
  ]) {
    assert.match(api, new RegExp(token))
  }
})

test('proxy scoring docs describe embedded credentials, quota and safety boundaries', () => {
  for (const token of ['/v3/', 'SCAM_KEY', '每日最多检测数', '受限 DNS 解析', '仅供参考']) {
    assert.match(docs, new RegExp(token))
  }
  assert.doesNotMatch(docs, /页面访问口令/)
  assert.doesNotMatch(docs, /评分服务 Base URL/)
})
