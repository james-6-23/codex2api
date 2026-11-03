import assert from 'node:assert/strict'
import { after, before, test } from 'node:test'
import { fileURLToPath } from 'node:url'
import { mkdir, mkdtemp } from 'node:fs/promises'
import { tmpdir } from 'node:os'
import path from 'node:path'
import { build } from 'vite'
import react from '@vitejs/plugin-react'
import tailwindcss from '@tailwindcss/vite'
import { chromium } from 'playwright'
const root = fileURLToPath(new URL('../', import.meta.url))
let artifactDir
let browser, script, css
const question = { id: 'candy', name: '糖果配对', prompt: '本地演示题目，最终只恢复数字结果。', expected_answer: '21', match_mode: 'exact', max_duration_ms: 20000, enabled: true }
const settings = { adaptive_enabled: false, pause_initial_seconds: 1800, pause_max_seconds: 172800, recovery_initial_seconds: 300, recovery_min_seconds: 120, revision: 1, enabled: false, gpt_model: 'gpt-6-astra', claude_model: 'claude-sonnet-4-6', reasoning_effort: 'high', judge_mode: 'answer', interval_seconds: 300, retry_seconds: 60, timeout_seconds: 180, concurrency: 4, retention: 200, new_accounts_enabled: true, action: 'none', degrade_threshold: 2, recover_threshold: 1, target_group_id: 0, questions: [question] }
const accounts = [{ account_id: 1, account_name: '本地 Codex 测试账号', channel: 'codex', enabled: true, latest_status: 'degraded', failed_streak: 2, passed_streak: 0, action: 'pause', next_due_at: '2026-10-15T13:00:00Z' }]
const run = { id: 11, account_id: 1, account_name: accounts[0].account_name, channel: 'codex', source: 'manual', status: 'degraded', config_revision: 1, model: 'gpt-6-astra', response_model: 'gpt-6-astra', reasoning_effort: 'high', judge_mode: 'answer', question, output: '29', error: '', duration_ms: 4120, input_tokens: 120, output_tokens: 10, reasoning_tokens: 8, created_at: '2026-10-15T12:00:00Z', started_at: '2026-10-15T12:00:00Z', completed_at: '2026-10-15T12:00:04Z', action: 'pause' }
before(async () => {
 artifactDir = process.env.INTELLIGENCE_TEST_ARTIFACT_DIR
  ? path.resolve(process.env.INTELLIGENCE_TEST_ARTIFACT_DIR)
  : await mkdtemp(path.join(tmpdir(), 'codex2api-intelligence-'))
 await mkdir(artifactDir, { recursive: true })
 const result = await build({ configFile: false, root, logLevel: 'silent', plugins: [react(), tailwindcss(), {
  name: 'intelligence-harness', enforce: 'pre',
  resolveId(id) { if (id === 'virtual:intelligence-harness') return '\0intelligence-harness'; if (/^(?:\.\.\/|\.\/)+api$/.test(id)) return '\0mock-api' },
  load(id) {
   if (id === '\0mock-api') return 'export const api = new Proxy({}, { get: (_, name) => (...args) => window.__api(name, args) }); export const getAdminKey = () => "fixture"; export const resetAdminAuthState = () => {}; export class AdminAPIError extends Error {}'
   if (id !== '\0intelligence-harness') return
   return `import React from 'react'; import {createRoot} from 'react-dom/client'; import {MemoryRouter} from 'react-router-dom';
   import IntelligenceCheck from ${JSON.stringify(path.join(root, 'src/pages/IntelligenceCheck.tsx'))};
   import ClaudeAccounts from ${JSON.stringify(path.join(root, 'src/pages/ClaudeAccounts.tsx'))};
   import Accounts from ${JSON.stringify(path.join(root, 'src/pages/Accounts.tsx'))};
   import {VisibleChannelsProvider} from ${JSON.stringify(path.join(root, 'src/visibleChannels.tsx'))};
   import AccountIntelligenceBar, {AccountIntelligenceActions} from ${JSON.stringify(path.join(root, 'src/components/AccountIntelligenceBar.tsx'))};
   import {useIntelligenceBars} from ${JSON.stringify(path.join(root, 'src/hooks/useIntelligenceBars.ts'))};
   import AccountHealthBar from ${JSON.stringify(path.join(root, 'src/components/AccountHealthBar.tsx'))};
   function BarHarness() { const {bars,error}=useIntelligenceBars('1',0); return React.createElement('div',{style:{maxWidth:'280px'}},React.createElement(AccountIntelligenceActions,{accountID:1,accountName:'fixture@example.test',bar:bars['1']}),React.createElement('div',{id:'reference-health'},React.createElement(AccountHealthBar,{buckets:[{success:9,failed:1}]})),React.createElement(AccountIntelligenceBar,{accountID:1,accountName:'fixture@example.test',bar:bars['1'],error})); }
   import {ToastProvider} from ${JSON.stringify(path.join(root, 'src/components/ToastProvider.tsx'))};
   import ${JSON.stringify(path.join(root, 'src/i18n.ts'))}; import ${JSON.stringify(path.join(root, 'src/index.css'))};
   createRoot(document.getElementById('root')).render(React.createElement(MemoryRouter, {initialEntries:[window.__initialPath||'/']}, React.createElement(ToastProvider,null,
   React.createElement('main',{style:{padding:'24px',maxWidth:'1400px',margin:'0 auto'}}, window.__showBar ? React.createElement(BarHarness) : null, window.__accountsPage ? React.createElement(VisibleChannelsProvider,null,React.createElement(window.__claudePage ? ClaudeAccounts : Accounts)) : React.createElement(IntelligenceCheck)))));`
  },
 }], resolve: { alias: { '@': path.join(root, 'src') } }, define: { 'process.env.NODE_ENV': '"production"' }, build: { write: false, minify: false, rollupOptions: { input: 'virtual:intelligence-harness' }, lib: { entry: 'virtual:intelligence-harness', formats: ['iife'], name: 'IntelligenceHarness' } } })
 const bundle = Array.isArray(result) ? result[0] : result
 script = bundle.output.find(item => item.type === 'chunk').code
 css = bundle.output.filter(item => item.type === 'asset' && item.fileName.endsWith('.css')).map(item => item.source).join('\n')
 browser = await chromium.launch({ headless: true })
})
after(async () => { await browser?.close() })
async function fixture(t, options = {}) {
 const page = await browser.newPage({ viewport: options.mobile ? { width: 390, height: 844 } : { width: 1280, height: 900 }, colorScheme: options.dark ? 'dark' : 'light' })
 page.setDefaultTimeout(5000); t.after(() => page.close())
 const errors = []; page.on('pageerror', e => errors.push(e.message)); t.after(() => assert.deepEqual(errors, []))
 await page.route('http://intelligence.test/**', route => route.fulfill({ contentType: 'text/html', body: '<!doctype html><html><head><meta name="viewport" content="width=device-width, initial-scale=1"></head><body><div id="root"></div></body></html>' }))
 await page.goto('http://intelligence.test/')
 await page.evaluate(({ settings, accounts, run, options }) => {
  localStorage.setItem("lang", "zh"); localStorage.setItem("codex2api:accounts:page-mode", "pool"); window.__claudePage = options.claudePage; window.__accountsPage = options.accountsPage; window.__initialPath = options.initialPath; window.__calls = []; window.__config = settings; window.__showBar = options.bar
  window.__bar = { enabled: options.detectionEnabled ?? true, action: 'none', records: ['normal', 'degraded', 'error', 'interrupted'].map((status, i) => ({ id: 10 + i, status, question_name: '糖果配对', duration_ms: 1200, completed_at: run.completed_at })) }
  if (options.dark) document.documentElement.classList.add('dark')
  window.__conflictActive = !!options.conflict
  if (options.conflict) window.__config = {...settings, action: 'switch_group', target_group_id: 1}
  window.__api = async (name, args) => {
   window.__calls.push({ name, args })
   const account = { id: 1, name: '本地 Codex 测试账号', email: 'fixture@example.test', account_type: 'oauth', status: 'active', enabled: true, health_tier: 'healthy', created_at: '2026-10-15T12:00:00Z', group_ids: [], models: ['gpt-6-astra'], detail_loaded: true, usage_percent_7d: 12, active_requests: 2, dynamic_concurrency_limit: 200, ...options.accountOverrides }
   if (name === 'getVisibleChannels') return { channels: ['codex', 'claude'] }
   if (name === 'getAccountsPage') return { accounts: [account], total: 1, page: 1, page_size: 20, facets: { tags: [], email_domains: [] }, summary: { total: 1 } }
   if (name === 'getAccount') return account
   if (name === 'getAccountHealthBars') return { buckets: {} }
   if (name === 'getIntelligenceBars') return { bars: { '1': window.__bar } }
   if (name === 'getAccountPageStats') return { stats: {} }
   if (name === 'getAccountLiveState') return { accounts: {} }
   if (name === 'getAPIKeys') return { keys: [] }
   if (name === 'getSettings') return {}
   if (name === 'getProxies') return { proxies: [] }
   if (name === 'getChannelMonitorBillingRates') return { rates: [] }
   if (name === 'getIntelligenceSettings') return window.__config
   if (name === 'listAccountGroups') return { groups: [{ id: 1, name: '隔离分组', channel: 'codex' }, { id: 2, name: 'Claude 隔离', channel: 'claude' }, { id: 3, name: 'Grok 隔离', channel: 'grok' }] }
   if (name === 'getIntelligenceAccounts') { if (options.error) throw new Error('本地模拟读取失败'); return { items: options.empty ? [] : accounts, total: options.empty ? 0 : 1, page: 1, page_size: 20 } }
   if (name === 'getIntelligenceRuns') { if (options.error) throw new Error('本地模拟读取失败'); return { items: options.empty ? [] : (options.pagination ? [{...run,id:args[0] === 2 ? 3 : 21,source:'automatic'},run].sort((a,b)=>b.id-a.id) : [run]), total: options.empty ? 0 : options.pagination ? 22 : 1, page: args[0], page_size: 20 } }
   if (name === 'getIntelligenceRun') return { run }
   if (name === 'saveIntelligenceSettings') {
    if (options.saveError) throw new Error('配置版本冲突')
    if (window.__conflictActive) throw Object.assign(new Error('intelligence check target group is incompatible: 1 enabled accounts belong to another channel'), {details: {code:'intelligence_check_target_conflict',target_group_id:1,target_group_channel:'codex',conflict_count:options.conflictCount || 1,conflicts:[{account_id:68876,account_name:'冲突 Claude 账号',channel:'claude'}]}})
    window.__config = { ...args[0], revision: args[0].revision + 1 }; return window.__config
   }
   if (name === 'createIntelligenceRuns') { if (options.createError) throw new Error('检测提交失败'); window.__bar = {...window.__bar,active_run_id:99,active_status:'queued'}; return { runs: [{...run,id:99,status:'queued'}] } }
   if (name === 'setIntelligenceAccounts') { if (options.toggleError) throw new Error('保存开关失败'); if (args[0].includes(68876) && !args[1]) window.__conflictActive=false; window.__bar.enabled=args[1]; return { updated: args[0].length } }
   throw new Error(`unexpected API ${name}`)
  }
 }, { settings, accounts, run, options })
 await page.addStyleTag({ content: css }); await page.addScriptTag({ content: script })
 if (!options.accountsPage) await page.getByRole('heading', { name: '降智检测', exact: true, includeHidden: true }).waitFor({ state: 'attached' })
 return page
}
test('configuration and questions share one page and preserve their save flow', async t => {
 const page = await fixture(t)
 assert.equal(await page.getByRole('tab').count(),0)
 const config = page.getByRole('region', {name:'检测配置'}), questions = page.getByRole('region', {name:'检测题库'})
 await questions.getByRole('button', {name:'编辑',exact:true}).click()
 assert.equal(await page.getByLabel('题目正文').inputValue(),question.prompt)
 const left=await config.boundingBox(),right=await questions.boundingBox()
 assert.ok(right.x>left.x && Math.abs(right.y-left.y)<1)
 await page.getByLabel('GPT / Codex 模型',{exact:true}).fill('gpt-6.1-sol')
 await page.getByLabel('题目正文').fill('第一行\n第二行')
 await page.getByRole('button',{name:'保存配置',exact:true}).click()
 await page.getByRole('button',{name:'保存配置',exact:true,disabled:true}).waitFor()
 assert.equal(await page.evaluate(()=>window.__config.gpt_model),'gpt-6.1-sol')
 assert.equal(await page.evaluate(()=>window.__config.questions[0].prompt),'第一行\n第二行')
 assert.equal(await page.evaluate(()=>window.__calls.some(c=>c.name==='getIntelligenceRuns')),false)
 await page.screenshot({path:path.join(artifactDir,'settings-light.png'),fullPage:true})
})

test('shared health bar dimensions, three colors, tooltip and record keyboard details',async t=>{
 const page=await fixture(t,{bar:true})
 const bar=page.getByLabel('最近 20 次降智检测，由旧到新')
 await bar.getByRole('button').first().waitFor()
 const dimensions=await page.evaluate(()=>{
  const health=document.querySelector('#reference-health [style]'),check=document.querySelector('[aria-label="最近 20 次降智检测，由旧到新"] [style]')
  return [health.getBoundingClientRect().height,check.getBoundingClientRect().height,health.className,check.className]
 })
 assert.equal(dimensions[0],6);assert.equal(dimensions[1],6);assert.equal(dimensions[2],dimensions[3])
 assert.deepEqual(await bar.locator('[style]').evaluateAll(els=>els.map(el=>el.style.backgroundColor)),['rgb(34, 197, 94)','rgb(239, 68, 68)','rgb(153, 27, 27)'])
 assert.match(await bar.innerText(),/50%/)
 const block=bar.getByRole('button',{name:/糖果配对 · 上游错误/})
 await block.hover();await page.getByRole('tooltip').waitFor()
 assert.match(await page.getByRole('tooltip').innerText(),/1.20 秒/)
 await block.focus();await page.keyboard.press('Enter');await page.getByRole('dialog').waitFor()
 assert.match(await page.getByRole('dialog').innerText(),/29/)
})

test('manual and automatic runs keep spinning until the batch reports completion',async t=>{
 const page=await fixture(t,{bar:true})
 await page.getByRole('button',{name:'立即检测',exact:true}).click()
 const running=page.getByRole('button',{name:'检测中',exact:true})
 await running.waitFor();assert.equal(await running.isDisabled(),true)
 assert.equal(await running.locator('.animate-spin').count(),1)
 await page.evaluate(()=>{window.__bar.active_status='running'})
 await page.waitForTimeout(4300)
 assert.equal(await running.locator('.animate-spin').count(),1)
 assert.equal(await page.evaluate(()=>window.__calls.filter(c=>c.name==='createIntelligenceRuns').length),1)
 await page.evaluate(()=>{window.__bar={...window.__bar,active_run_id:undefined,active_status:undefined,records:[...window.__bar.records,{id:99,status:'normal'}]}})
 await page.getByRole('button',{name:'立即检测',exact:true}).waitFor({timeout:6000})
 await page.evaluate(()=>{window.__bar={...window.__bar,active_run_id:100,active_status:'running'}})
 await running.waitFor({timeout:6000});assert.equal(await running.locator('.animate-spin').count(),1)
 await page.evaluate(()=>{window.__bar={...window.__bar,active_run_id:undefined,active_status:undefined,records:[{id:100,status:'normal'}]}})
 await page.getByRole('button',{name:'立即检测',exact:true}).waitFor({timeout:6000})
})

test('dark mobile history cards, pagination, automatic toggle and empty/error states',async t=>{
 const page=await fixture(t,{dark:true,mobile:true,bar:true,pagination:true,detectionEnabled:false})
 const bar=page.getByLabel('最近 20 次降智检测，由旧到新')
 await page.waitForFunction(()=>window.__calls.some(c=>c.name==='getIntelligenceBars'))
 assert.equal(await bar.count(),0)
 assert.equal(await page.locator('#reference-health').isVisible(),true)
 await page.getByRole('button',{name:'检测记录',exact:true}).click()
 const dialog=page.getByRole('dialog');await dialog.locator('article').first().waitFor()
 const toggle=dialog.getByRole('switch',{name:'此账号自动检测'})
 assert.equal(await toggle.isChecked(),false)
 await toggle.click()
 await bar.waitFor({state:'attached'})
 assert.match(await dialog.innerText(),/fixture@example.test/)
 assert.match(await dialog.innerText(),/自动/);assert.match(await dialog.innerText(),/判定原因：答案不匹配/)
 assert.match(await dialog.innerText(),/预期答案：21/);assert.match(await dialog.innerText(),/29/)
 assert.match(await dialog.locator('article').first().innerText(),/#21/)
 await dialog.getByRole('button',{name:'下一页'}).click()
 await page.waitForFunction(()=>window.__calls.some(c=>c.name==='getIntelligenceRuns'&&c.args[0]===2))
 await dialog.getByRole('switch',{name:'此账号自动检测'}).click()
 await page.waitForFunction(()=>window.__calls.some(c=>c.name==='setIntelligenceAccounts'&&c.args[1]===false))
 await bar.waitFor({state:'detached'})
 assert.ok(await page.evaluate(()=>document.documentElement.scrollWidth<=window.innerWidth+1))
 await page.screenshot({path:path.join(artifactDir,'history-dark-mobile.png'),fullPage:true})
 await page.keyboard.press('Escape')
 assert.equal(await page.getByRole('button',{name:'立即检测',exact:true}).isEnabled(),true)
 assert.equal(await page.getByRole('button',{name:'检测记录',exact:true}).isVisible(),true)
 const config=await page.getByRole('region',{name:'检测配置'}).boundingBox(),questions=await page.getByRole('region',{name:'检测题库'}).boundingBox()
 assert.ok(questions.y>config.y)
 await page.screenshot({path:path.join(artifactDir,'settings-dark-mobile.png'),fullPage:true})
 const empty=await fixture(t,{initialPath:'/?account_id=1',empty:true});await empty.getByText(/暂无检测记录，可在/).waitFor()
 const error=await fixture(t,{initialPath:'/?account_id=1',error:true});await error.getByRole('alert').filter({hasText:'本地模拟读取失败'}).waitFor()
})

test('save errors keep drafts and failed requests restore the manual action',async t=>{
 const page=await fixture(t,{saveError:true,bar:true,createError:true})
 await page.getByLabel('GPT / Codex 模型',{exact:true}).fill('gpt-6-sol')
 await page.getByRole('button',{name:'保存配置',exact:true}).click()
 await page.getByText('配置版本冲突',{exact:true}).waitFor()
 assert.equal(await page.getByLabel('GPT / Codex 模型',{exact:true}).inputValue(),'gpt-6-sol')
 assert.equal(await page.evaluate(()=>window.__config.gpt_model),'gpt-6-astra')
 await page.getByRole('button',{name:'立即检测',exact:true}).click()
 await page.getByText('检测提交失败',{exact:true}).waitFor()
 assert.equal(await page.getByRole('button',{name:'立即检测',exact:true}).isEnabled(),true)
})

test('conflicting accounts can be disabled explicitly and the unsaved configuration retried', async t => {
 const page = await fixture(t, {conflict:true, mobile:true})
 await page.getByLabel('GPT / Codex 模型',{exact:true}).fill('gpt-6-sol')
 await page.getByRole('button',{name:'保存配置',exact:true}).click()
 const conflict = page.getByRole('alert').filter({hasText:'配置尚未保存'})
 await conflict.waitFor()
 assert.match(await conflict.innerText(),/#68876 · 冲突 Claude 账号 · claude/)
 assert.match(await conflict.innerText(),/隔离分组（codex）/)
 assert.equal(await page.evaluate(()=>window.__calls.filter(c=>c.name==='setIntelligenceAccounts').length),0)
 assert.ok(await page.evaluate(()=>document.documentElement.scrollWidth<=window.innerWidth+1))
 await page.screenshot({path:path.join(artifactDir,'settings-conflict-mobile.png'),fullPage:true})
 await conflict.getByRole('button',{name:'关闭以上 1 个账号的自动检测'}).click()
 await conflict.waitFor({state:'detached'})
 assert.deepEqual(await page.evaluate(()=>window.__calls.find(c=>c.name==='setIntelligenceAccounts').args),[[68876],false])
 assert.equal(await page.getByLabel('GPT / Codex 模型',{exact:true}).inputValue(),'gpt-6-sol')
 assert.equal(await page.evaluate(()=>window.__config.gpt_model),'gpt-6-astra')
 await page.getByRole('button',{name:'保存配置',exact:true}).click()
 await page.getByRole('button',{name:'保存配置',exact:true,disabled:true}).waitFor()
 assert.equal(await page.evaluate(()=>window.__config.gpt_model),'gpt-6-sol')
})

test('failed conflict resolution keeps the account list and configuration draft', async t => {
 const page = await fixture(t, {conflict:true, toggleError:true, conflictCount:201})
 await page.getByLabel('GPT / Codex 模型',{exact:true}).fill('gpt-6-sol')
 await page.getByRole('button',{name:'保存配置',exact:true}).click()
 const conflict = page.getByRole('alert').filter({hasText:'配置尚未保存'})
 await conflict.waitFor()
 assert.match(await conflict.innerText(),/处理后重新保存可查看剩余冲突/)
 await conflict.getByRole('button',{name:'关闭以上 1 个账号的自动检测'}).click()
 await page.getByText('保存开关失败',{exact:true}).waitFor()
 assert.equal(await conflict.isVisible(),true)
 assert.equal(await page.getByLabel('GPT / Codex 模型',{exact:true}).inputValue(),'gpt-6-sol')
})
async function assertConcurrencyInline(scope) {
 const concurrency = scope.locator('span[title*="当前正在处理 2 个请求"]').first()
 const detect = scope.getByRole('button', {name: '立即检测', exact: true})
 await Promise.all([concurrency.waitFor(), detect.waitFor()])
 const [counter, action] = await Promise.all([concurrency.boundingBox(), detect.boundingBox()])
 assert.ok(counter && action, 'concurrency and action must both be visible')
 assert.ok(Math.abs(counter.y + counter.height/2 - action.y - action.height/2) < 2, 'concurrency stays on the action row')
}
async function assertActionsFollowStatus(scope) {
 const detect = scope.getByRole('button', {name: '立即检测', exact: true})
 const history = scope.getByRole('button', {name: '检测记录', exact: true})
 await detect.waitFor()
 const group = detect.locator('..').locator('..')
 const badge = group.locator('[data-slot="badge"]').first()
 const [statusBox, detectBox, historyBox] = await Promise.all([badge.boundingBox(), detect.boundingBox(), history.boundingBox()])
 assert.ok(detectBox.x > statusBox.x + statusBox.width, 'detect follows the status badge')
 assert.ok(historyBox.x > detectBox.x, 'history follows detect')
 assert.ok(Math.abs(detectBox.y + detectBox.height/2 - statusBox.y - statusBox.height/2) < 2, 'actions share the badge row')
 assert.equal(await group.locator('button').count(), 2)
 const actions = scope.getByRole('group', {name: '检测操作'})
 assert.equal(await actions.getByRole('button').count(), 2)
 for (const button of [detect, history]) {
  const appearance = await button.evaluate(el => {
   const style = getComputedStyle(el)
   return { width: el.getBoundingClientRect().width, height: el.getBoundingClientRect().height, iconWidth: el.querySelector('svg').getBoundingClientRect().width, background: style.backgroundColor }
  })
  assert.equal(appearance.background, 'rgba(0, 0, 0, 0)', 'actions have no filled bubble')
  assert.equal(appearance.width,18); assert.equal(appearance.height,18)
  assert.equal(appearance.iconWidth,12)
 }
 assert.equal(await actions.evaluate(el => getComputedStyle(el).backgroundColor), 'rgba(0, 0, 0, 0)', 'the actions do not share a capsule background')

}
test('account table, card and detail show their independent intelligence history', async t => {
 const page = await fixture(t, { accountsPage: true })
 await page.getByRole('button', { name: '表格', exact: true }).click()
 await page.getByLabel('最近 20 次降智检测，由旧到新').waitFor()
 assert.equal(await page.getByRole('button', { name: /糖果配对 · 上游错误/ }).count(), 1)
 await assertActionsFollowStatus(page)
 await assertConcurrencyInline(page)
 await page.locator('[data-account-state-cell="status"]').screenshot({ path: path.join(artifactDir, 'status-compact.png') })
 await page.screenshot({ path: path.join(artifactDir, 'accounts-table.png'), fullPage: true })
 await page.getByRole('button', { name: '卡片', exact: true }).click()
 await page.locator('.codex-account-card').waitFor()
 assert.equal(await page.locator('.codex-account-card').getByLabel('最近 20 次降智检测，由旧到新').count(), 1)
 await assertActionsFollowStatus(page.locator('.codex-account-card'))
 await assertConcurrencyInline(page.locator('.codex-account-card'))
 await page.screenshot({ path: path.join(artifactDir, 'accounts-card.png'), fullPage: true })
 await page.setViewportSize({width:390,height:844})
 await assertActionsFollowStatus(page.locator('.codex-account-card'))
 await assertConcurrencyInline(page.locator('.codex-account-card'))
 assert.ok(await page.evaluate(() => document.documentElement.scrollWidth <= innerWidth + 1))
 await page.screenshot({ path: path.join(artifactDir, 'accounts-card-mobile.png'), fullPage: true })
 await page.setViewportSize({width:1280,height:900})
 await page.locator('.codex-account-card__actions').getByRole('button', { name: '查看详情', exact: true }).click()
 // Existing account detail opens the quick-config sheet above the detail sheet.
 await page.keyboard.press('Escape')
 await page.getByRole('dialog').getByLabel('最近 20 次降智检测，由旧到新').waitFor()
 await assertActionsFollowStatus(page.getByRole('dialog'))
 await page.screenshot({ path: path.join(artifactDir, 'accounts-detail.png'), fullPage: true })
 assert.ok(await page.evaluate(() => window.__calls.filter(c => c.name === 'getIntelligenceBars').every(c => JSON.stringify(c.args[0])==='[1]')))
})

test('Claude account table and detail receive batch intelligence history', async t => {
 const page = await fixture(t, { accountsPage: true, claudePage: true, accountOverrides: { claude_api: true, claude_auth_kind: 'api_key' } })
 await page.getByLabel('最近 20 次降智检测，由旧到新').waitFor()
 await assertActionsFollowStatus(page)
 await assertConcurrencyInline(page)
 await page.getByRole('row').filter({ hasText: 'fixture@example.test' }).click()
 await page.getByRole('dialog').getByLabel('最近 20 次降智检测，由旧到新').waitFor()
 await assertActionsFollowStatus(page.getByRole('dialog'))
 assert.ok(await page.evaluate(() => window.__calls.filter(c => c.name === 'getIntelligenceBars').every(c => JSON.stringify(c.args[0])==='[1]')))
})


test('adaptive settings can be enabled and saved with an independent long cap', async t => {
 const page = await fixture(t)
 await page.getByRole('switch', {name:'自适应检测',exact:true}).click()
 assert.equal(await page.getByLabel('暂停最大间隔（秒）',{exact:true}).inputValue(),'172800')
 await page.getByLabel('暂停最大间隔（秒）',{exact:true}).fill('259200')
 await page.getByRole('button',{name:'保存配置',exact:true}).click()
 await page.getByRole('button',{name:'保存配置',exact:true,disabled:true}).waitFor()
 assert.equal(await page.evaluate(()=>window.__config.adaptive_enabled),true)
 assert.equal(await page.evaluate(()=>window.__config.pause_max_seconds),259200)
 await page.screenshot({path:path.join(artifactDir,'settings-adaptive.png'),fullPage:true})
})

test('account bar shows recovery progress and persisted next probe', async t => {
 const page = await fixture(t,{bar:true})
 await page.evaluate(()=>{window.__bar={...window.__bar,action:'pause',probe_phase:'recovering',passed_streak:1,next_due_at:'2026-11-03T12:00:00Z'}})
 await page.getByText(/恢复确认 · 连续通过 1 次 · 下次/).waitFor({timeout:6000})
 await page.screenshot({path:path.join(artifactDir,'account-recovering.png'),fullPage:true})
})
