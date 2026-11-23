import assert from 'node:assert/strict'
import { readFile } from 'node:fs/promises'
import { after, before, test } from 'node:test'
import path from 'node:path'
import { fileURLToPath } from 'node:url'
import { crc32, inflateRawSync } from 'node:zlib'
import { build } from 'vite'
import react from '@vitejs/plugin-react'
import tailwindcss from '@tailwindcss/vite'
import { chromium } from 'playwright'

const root = fileURLToPath(new URL('../', import.meta.url))
let browser, script, css

before(async () => {
  const result = await build({
    configFile: false,
    root,
    logLevel: 'silent',
    plugins: [react(), tailwindcss(), {
      name: 'prompt-request-context-fixture',
      enforce: 'pre',
      resolveId(id) {
        if (id === 'virtual:prompt-request-context-fixture') return '\0prompt-request-context-fixture'
        if (id === '../api') return '\0prompt-request-context-api'
      },
      load(id) {
        if (id === '\0prompt-request-context-api') {
          return 'export const api = new Proxy({}, {get: (_, name) => (...args) => window.fixtureAPI(name, args)}); export class AdminAPIError extends Error {}'
        }
        if (id !== '\0prompt-request-context-fixture') return
        return `import React from 'react'; import { createRoot } from 'react-dom/client';
          import { MemoryRouter } from 'react-router-dom';
          import Records from ${JSON.stringify(path.join(root, 'src/pages/PromptRequestRecords.tsx'))};
          import ${JSON.stringify(path.join(root, 'src/i18n.ts'))};
          import ${JSON.stringify(path.join(root, 'src/index.css'))};
          createRoot(document.getElementById('root')).render(React.createElement(MemoryRouter,
            {initialEntries:[window.fixtureLocation]}, React.createElement(Records, {refresh:0}))); `
      },
    }],
    resolve: { alias: { '@': path.join(root, 'src') } },
    define: { 'process.env.NODE_ENV': '"production"' },
    build: {
      write: false,
      minify: false,
      rollupOptions: { input: 'virtual:prompt-request-context-fixture' },
      lib: { entry: 'virtual:prompt-request-context-fixture', formats: ['iife'], name: 'RequestReviewFixture' },
    },
  })
  const bundle = Array.isArray(result) ? result[0] : result
  script = bundle.output.find((item) => item.type === 'chunk').code
  css = bundle.output.filter((item) => item.type === 'asset' && item.fileName.endsWith('.css')).map((item) => item.source).join('\n')
  browser = await chromium.launch({
    executablePath: process.env.PROMPT_TEST_CHROMIUM || undefined,
    args: ['--no-sandbox'],
  })
})

after(async () => browser?.close())

async function reviewFixture(context, detail, location = '/prompt-filter/requests', replay) {
  const page = await browser.newPage()
  page.setDefaultTimeout(8000)
  context.after(() => page.close())
  const errors = []
  page.on('pageerror', (error) => errors.push(error.message))
  context.after(() => assert.deepEqual(errors, []))
  await page.route('http://request-review.test/', (route) => route.fulfill({
    contentType: 'text/html', body: '<!doctype html><html><body><div id="root"></div></body></html>',
  }))
  await page.goto('http://request-review.test/')
  await page.evaluate(({ detail, location, replay }) => {
    localStorage.setItem('lang', 'zh')
    window.fixtureLocation = location
    window.fixtureImageCalls = []
    window.fixtureTaskDetailCalls = []
    window.fixtureAPI = async (name, args) => {
      const { id, created_at, endpoint, model, api_key_name, action, status, transport } = detail
      const record = { id, created_at, endpoint, model, api_key_name, action, status, transport }
      if (name === 'listPromptRequestConversations') {
        return { groups: [{ ...record, rounds: 1 }], next_before: 0, enabled: true, retention_days: 30 }
      }
      if (name === 'listPromptRequestRounds') return { rounds: [record], total: 1, next_before: 0 }
      if (name === 'listPromptRequestTasks' && replay?.pages[args[1]]) return replay.pages[args[1]]
      if (name === 'getPromptRequestTask' && replay?.tasks[args[0]]) {
        window.fixtureTaskDetailCalls.push(args[0])
        return replay.tasks[args[0]]
      }
      if (name === 'getPromptRequest' && args[0] === id) return detail
      if (name === 'getPromptRequest' && replay?.requests?.[args[0]]) return replay.requests[args[0]]
      if (name === 'getPromptRequestImage') {
        window.fixtureImageCalls.push(args.slice(0, 2))
        const bytes = Uint8Array.from(atob('iVBORw0KGgoAAAANSUhEUgAAAAEAAAABCAQAAAC1HAwCAAAAC0lEQVR42mNk+A8AAQUBAScY42YAAAAASUVORK5CYII='), (c) => c.charCodeAt(0))
        return new Blob([bytes], { type: 'image/png' })
      }
      throw new Error('Unexpected API: ' + name)
    }
  }, { detail, location, replay })
  await page.addStyleTag({ content: css })
  await page.addScriptTag({ content: script })
  return page
}

const record = {
  id: 26, created_at: '2026-11-22T01:00:00Z', endpoint: '/v1/responses',
  model: 'fixture', api_key_name: 'fixture', action: 'allow', status: 200, transport: 'http',
  images: [],
}

function readArchive(buffer) {
  const end = buffer.lastIndexOf(Buffer.from([0x50, 0x4b, 0x05, 0x06]))
  assert.ok(end >= 0, 'download must contain a ZIP central directory')
  const count = buffer.readUInt16LE(end + 10)
  let offset = buffer.readUInt32LE(end + 16)
  const entries = []
  for (let i = 0; i < count; i++) {
    assert.equal(buffer.readUInt32LE(offset), 0x02014b50)
    const method = buffer.readUInt16LE(offset + 10)
    const size = buffer.readUInt32LE(offset + 20)
    const nameLength = buffer.readUInt16LE(offset + 28)
    const name = buffer.toString('utf8', offset + 46, offset + 46 + nameLength)
    const local = buffer.readUInt32LE(offset + 42)
    assert.equal(buffer.readUInt32LE(local), 0x04034b50)
    const start = local + 30 + buffer.readUInt16LE(local + 26) + buffer.readUInt16LE(local + 28)
    assert.ok(method === 0 || method === 8)
    const payload = buffer.subarray(start, start + size)
    const body = method === 8 ? inflateRawSync(payload) : payload
    assert.equal(crc32(body), buffer.readUInt32LE(offset + 16))
    entries.push({ name, body: body.toString('utf8') })
    offset += 46 + nameLength + buffer.readUInt16LE(offset + 30) + buffer.readUInt16LE(offset + 32)
  }
  return entries
}

test('renders per-round provenance and Skill evidence with one default disclosure for a covered body', async (context) => {
  const developer = 'Fixture developer text\nSecond line.'
  const skillBody = 'Fixture Skill text.'
  const developerBlocks = developer.split('\n')
  const page = await reviewFixture(context, {
    ...record,
    text: JSON.stringify({ input: [{ role: 'developer', content: developerBlocks.map((text) => ({ type: 'input_text', text })) }] }),
    changes: { previous_id: 25, repeated_items: 2, new_items: 1, historical_images: 0, image_indices: [],
      entries: developerBlocks.map((text) => ({ kind: 'text', label: 'developer', text })) },
    prompt_context: { sections: [
      { kind: 'developer_prompt', state: 'new', text: developer, source: 'developer_message' },
      { kind: 'loaded_skill', state: 'new', text: skillBody, name: 'fixture', source: 'tool_result',
        evidence: 'successful_read', source_request_id: 27 },
      { kind: 'attempted_skill', state: 'received', name: 'other', evidence: 'read_failed' },
      { kind: 'available_skills', state: 'inherited', skills: [{ name: 'listing' }] },
    ] },
    langfuse_trace_url: 'https://langfuse.example.test/project/example/traces/1',
    result: { response: { status: 'interrupted', text: 'Fixture interrupted reply.' } },
  }, '/prompt-filter/requests?request=26')
  const dialog = page.getByRole('dialog')
  const prompts = dialog.getByRole('region', { name: '本轮提示词与 Skills' })
  await prompts.getByText('未见独立系统字段').waitFor()
  const developerBlock = prompts.locator('summary').filter({ hasText: '开发者提示词' })
  assert.equal(await dialog.locator('summary').filter({ hasText: '开发者提示词' }).count(), 1)
  assert.equal(await prompts.getByText('Fixture developer text', { exact: false }).isVisible(), false)
  await developerBlock.click()
  await prompts.getByText('来源：developer 消息').waitFor()
  assert.equal(await prompts.locator('pre').filter({ hasText: 'Fixture developer text' }).textContent(), developer)
  await prompts.locator('summary').filter({ hasText: '已加载 Skill' }).click()
  await prompts.getByText('证据：工具结果确认读取成功').waitFor()
  await prompts.getByText('证据来自请求 #27').waitFor()
  await prompts.locator('summary').filter({ hasText: '尝试读取 Skill' }).click()
  await prompts.getByText('证据：工具结果显示读取失败').waitFor()
  await prompts.locator('summary').filter({ hasText: '可用 Skills' }).click()
  await prompts.getByText('listing').waitFor()
  await dialog.getByText('流式中断').waitFor()
  const link = dialog.getByRole('link', { name: '在 Langfuse 中查看' })
  assert.equal(await link.getAttribute('rel'), 'noopener noreferrer')
  assert.match(await link.getAttribute('href'), /^https:\/\/langfuse\.example\.test\//)
  await dialog.locator('summary').filter({ hasText: '查看完整请求' }).click()
  await dialog.getByRole('heading', { name: '完整请求文字' }).waitFor()
  assert.equal(await dialog.locator('summary').filter({ hasText: '开发者提示词' }).count(), 2)
})

test('opens a deep-linked request by ID and ignores a non-HTTP trace URL', async (context) => {
  const page = await reviewFixture(context, {
    ...record, text: '{}', prompt_context: { sections: [] }, langfuse_trace_url: 'javascript:alert(1)',
  }, '/prompt-filter/requests?request=26')
  const dialog = page.getByRole('dialog')
  await dialog.getByRole('heading', { name: '请求详情 · #26' }).waitFor()
  await dialog.getByText('未观察到加载').waitFor()
  assert.equal(await dialog.getByRole('link', { name: '在 Langfuse 中查看' }).count(), 0)
  await page.keyboard.press('Escape')
  await dialog.waitFor({ state: 'hidden' })
})

test('keeps a metadata-only tool receipt readable without creating a client-side Skill badge', async (context) => {
  const page = await reviewFixture(context, {
    ...record,
    text: JSON.stringify({ input: [{ role: 'user', content: 'Read the fixture.' }] }),
    prompt_context: { sections: [
      { kind: 'attempted_skill', state: 'received', source: 'tool_result', source_request_id: 27,
        name: 'metadata-only', path: '/skills/metadata-only/SKILL.md', evidence: 'body_unavailable' },
    ] },
    result: { tool_results: [{
      call_id: 'metadata-read', name: 'exec_command', from_request_id: 27,
      arguments: JSON.stringify({ cmd: 'cat /skills/metadata-only/SKILL.md' }),
      text: JSON.stringify({ exit_code: 0, output: '---\nname: metadata-only\n---' }),
    }] },
  }, '/prompt-filter/requests?request=26')
  const dialog = page.getByRole('dialog')
  const results = dialog.getByRole('heading', { name: '本轮结果', exact: true }).locator('..')
  await results.getByText('name: metadata-only', { exact: false }).waitFor()
  assert.equal(await results.locator('summary').filter({ hasText: '已加载 Skill' }).count(), 0)
})

test('keeps a received system prompt folded on a round without a baseline', async (context) => {
  const system = 'Fixture system prompt.\nKeep the second line.'
  const user = 'Fixture user prose.'
  const page = await reviewFixture(context, {
    ...record,
    text: JSON.stringify({ instructions: system, input: [{ role: 'user', content: user }] }),
    prompt_context: { sections: [
      { kind: 'system_prompt', state: 'received', source: 'instructions', text: system },
    ] },
  }, '/prompt-filter/requests?request=26')
  const dialog = page.getByRole('dialog')
  const prompts = dialog.getByRole('region', { name: '本轮提示词与 Skills' })
  await dialog.getByText(user, { exact: false }).waitFor()
  const summary = prompts.locator('summary').filter({ hasText: '系统提示词' })
  await summary.getByText('本轮收到').waitFor()
  assert.equal(await dialog.locator('summary').filter({ hasText: '系统提示词' }).count(), 1)
  assert.equal(await prompts.locator('pre').isVisible(), false)
  await summary.click()
  await prompts.getByText('来源：instructions 字段').waitFor()
  assert.equal(await prompts.locator('pre').textContent(), system)
  await dialog.locator('summary').filter({ hasText: '查看完整请求' }).click()
  await dialog.getByRole('heading', { name: '完整请求文字' }).waitFor()
  assert.equal(await dialog.locator('summary').filter({ hasText: '系统提示词' }).count(), 2)
})

test('replays a client-observed user message, skills, model steps and final Markdown separately', async (context) => {
  const image = { index: 0, source: 'embedded', label: 'Fixture attachment' }
  const answer = '**已上线。**\n\n- [请求记录](https://example.test/admin)\n\n![附件](https://example.test/remote.png)\n\n[非网页链接](javascript:alert(1))'
  const summary = {
    id: 26, started_at: record.created_at, request_ids: [26, 27], status: 'completed',
    user_text: '部署请求记录。\n只替换应用容器。', user_request_id: 26, user_images: [image],
    final_preview: answer, final_status: 'completed', final_source: 'client', duration_ms: 560000, timing_source: 'client',
  }
  const replay = {
    pages: { 0: { tasks: [summary], total_requests: 2, next_before: 0 } },
    tasks: { 26: {
      ...summary,
      user: {
        text: '部署请求记录。\n只替换应用容器。', request_id: 26, source: 'client',
        images: [image],
      },
      steps: [
        { request_id: 26, created_at: record.created_at, model: record.model, endpoint: record.endpoint,
          action: 'allow', status: 200, tools: [
            { call_id: 'command', name: 'exec_command', arguments: '{"cmd":"npm test"}',
              result: '{"output":"Fixture command passed."}', from_request_id: 27 },
          ] },
        { request_id: 27, created_at: '2026-11-22T01:05:00Z', model: record.model, endpoint: record.endpoint,
          action: 'allow', status: 200, tools: [] },
      ],
      client_steps: [
        { name: 'client-skill', kind: 'skill', status: 'loaded' },
        { name: 'exec_command', kind: 'tool', status: 'observed' },
        ...Array.from({ length: 5 }, (_, index) => ({ name: 'client-tool-' + (index + 3), kind: 'tool', status: 'observed' })),
      ],
      skills: [
        { kind: 'loaded_skill', state: 'new', name: 'example-skill', evidence: 'successful_read', source: 'tool_result',
          source_request_id: 26, text: 'Read the recorded instructions.' },
        { kind: 'available_skills', state: 'inherited', skills: [{ name: 'catalog-only' }] },
      ],
      final: { text: answer, status: 'completed', source: 'client' },
      duration_ms: 560000,
      timing_source: 'client',
    } },
  }
  const page = await reviewFixture(context, { ...record, text: '{}', prompt_context: { sections: [] } }, undefined, replay)
  await page.getByRole('button', { name: '查看会话' }).click()
  const dialog = page.getByRole('dialog')
  await dialog.getByRole('heading', { name: '任务回放' }).waitFor()
  const user = dialog.getByRole('heading', { name: '用户发送了什么' }).locator('..')
  await user.waitFor()
  await dialog.getByRole('heading', { name: '用户看到的最终答复' }).waitFor()
  await dialog.getByText('只替换应用容器。', { exact: false }).waitFor()
  assert.equal(await user.getByRole('button', { name: '展开全文', exact: true }).count(), 0)
  await dialog.getByRole('img', { name: '图片 1' }).waitFor()
  assert.deepEqual(await page.evaluate(() => window.fixtureImageCalls), [[26, 0]])
  assert.deepEqual(await page.evaluate(() => window.fixtureTaskDetailCalls), [])
  const activity = dialog.getByRole('heading', { name: 'Codex 做了什么' }).locator('..')
  assert.equal(await activity.count(), 0)
  await dialog.getByRole('button', { name: '展开执行过程', exact: true }).click()
  await activity.waitFor()
  assert.equal(await dialog.getByText('catalog-only').count(), 0)
  const clientSkillButton = activity.getByRole('button', { name: 'client-skill', exact: true })
  const capturedSkillButton = activity.getByRole('button', { name: 'example-skill', exact: true })
  await clientSkillButton.waitFor()
  assert.equal(await clientSkillButton.locator('svg.lucide-download').count(), 0)
  await capturedSkillButton.locator('svg.lucide-download').waitFor()
  const clientSteps = activity.getByRole('list', { name: '客户端执行步骤' })
  await clientSteps.getByText('已加载 · client-skill', { exact: true }).waitFor()
  assert.equal(await clientSteps.getByRole('listitem').count(), 5)
  await clientSteps.getByText('client-tool-5', { exact: true }).waitFor()
  assert.equal(await clientSteps.getByText('client-tool-6', { exact: true }).count(), 0)
  const clientDisclosure = clientSteps.locator('..')
  await clientDisclosure.getByRole('button', { name: '展开其余 2 条', exact: true }).click()
  assert.equal(await clientSteps.getByRole('listitem').count(), 7)
  await clientSteps.getByText('client-tool-7', { exact: true }).waitFor()
  await clientDisclosure.getByRole('button', { name: '收起其余步骤', exact: true }).click()
  assert.equal(await clientSteps.getByRole('listitem').count(), 5)
  assert.equal(await activity.getByText('查看加载证据', { exact: true }).count(), 0)
  await capturedSkillButton.click()
  const capturedSkill = page.getByRole('dialog', { name: 'example-skill', exact: true })
  await capturedSkill.getByText('Read the recorded instructions.', { exact: true }).waitFor()
  await page.keyboard.press('Escape')
  await capturedSkill.waitFor({ state: 'hidden' })
  await clientSkillButton.click()
  const clientSkill = page.getByRole('dialog', { name: 'client-skill', exact: true })
  await clientSkill.getByText('未记录 Skill 正文', { exact: true }).waitFor()
  assert.ok(await clientSkill.getByRole('button', { name: '下载', exact: true }).isDisabled())
  assert.equal(await clientSkill.locator('pre').count(), 0)
  await page.keyboard.press('Escape')
  await clientSkill.waitFor({ state: 'hidden' })
  await dialog.getByText('整轮用时 9 分钟 20 秒').waitFor()
  assert.equal(await dialog.getByText('客户端记录', { exact: true }).count(), 1)
  await dialog.locator('strong').filter({ hasText: '已上线。' }).waitFor()
  const finalLink = dialog.getByRole('link', { name: '请求记录' })
  assert.equal(await finalLink.getAttribute('href'), 'https://example.test/admin')
  assert.equal(await finalLink.getAttribute('rel'), 'noopener noreferrer')
  assert.equal(await dialog.getByRole('img', { name: '附件' }).count(), 0)
  assert.equal(await dialog.getByRole('link', { name: '非网页链接' }).count(), 0)
  await dialog.locator('summary').filter({ hasText: '网关请求与工具细节' }).click()
  await dialog.locator('summary').filter({ hasText: '#26 · 200' }).click()
  await dialog.getByText('Fixture command passed.', { exact: false }).waitFor()
  await dialog.getByText('结果由请求 #27 回传').waitFor()
  await dialog.getByText('未记录模型答复', { exact: true }).waitFor()
  await dialog.getByRole('button', { name: '查看请求 #26' }).click()
  await dialog.getByRole('region', { name: '本轮提示词与 Skills' }).waitFor()
})

test('places images inside the user message and opens an image-only preview', async (context) => {
  const photo = (index) => ({ index, source: 'embedded', label: '$.input[0].content[' + index + '].image_url' })
  const text = '请看这些图片。'
  const tasks = [
    { id: 26, started_at: record.created_at, request_ids: [26], user_text: text,
      user_request_id: 26, user_images: [photo(2), photo(7)] },
    { id: 27, started_at: '2026-11-22T01:01:00Z', request_ids: [27],
      user_request_id: 27, user_images: [photo(0)] },
  ]
  const page = await reviewFixture(context, { ...record, text: '{}' }, undefined, {
    pages: { 0: { tasks, total_requests: 2, next_before: 0 } },
    tasks: {},
  })
  await page.getByRole('button', { name: '查看会话' }).click()
  const replay = page.getByRole('dialog', { name: '任务回放', exact: true })
  const userArea = (id) => replay.getByRole('article', { name: '任务 #' + id, exact: true })
    .getByRole('region', { name: '用户发送了什么', exact: true })
  const bubble = userArea(26).locator(':scope > div').first()
  const userText = bubble.getByText(text, { exact: true })
  await userText.waitFor()
  const firstPhoto = bubble.getByRole('img', { name: '图片 3', exact: true })
  const secondPhoto = bubble.getByRole('img', { name: '图片 8', exact: true })
  await firstPhoto.waitFor()
  await secondPhoto.waitFor()
  assert.ok((await firstPhoto.boundingBox()).y < (await userText.boundingBox()).y)
  assert.equal(await userArea(26).locator(':scope > div').count(), 1)
  assert.equal(await replay.getByText('用户附图', { exact: true }).count(), 0)

  const imageOnly = userArea(27).locator(':scope > div').first()
  await imageOnly.scrollIntoViewIfNeeded()
  await imageOnly.getByRole('img', { name: '图片 1', exact: true }).waitFor()
  assert.equal(await userArea(27).getByText('未记录用户发送内容', { exact: true }).count(), 0)

  const openPhoto = bubble.getByRole('button', { name: '图片 8', exact: true })
  const originalSource = await secondPhoto.getAttribute('src')
  await openPhoto.click()
  const viewer = page.getByRole('dialog', { name: '图片 8', exact: true })
  const fullPhoto = viewer.getByRole('img', { name: '图片 8', exact: true })
  await fullPhoto.waitFor()
  await fullPhoto.evaluate((image) => image.decode())
  assert.equal(await fullPhoto.getAttribute('src'), originalSource)
  assert.equal(await viewer.getByRole('img').count(), 1)
  assert.equal(await viewer.getByRole('button').count(), 0)

  await fullPhoto.click()
  assert.ok(await viewer.isVisible())
  await page.mouse.click(8, 8)
  await viewer.waitFor({ state: 'hidden' })
  assert.ok(await replay.isVisible())
  assert.equal(await openPhoto.evaluate((button) => button === document.activeElement), true)

  await openPhoto.click()
  await viewer.waitFor()
  await page.keyboard.press('Escape')
  await viewer.waitFor({ state: 'hidden' })
  assert.ok(await replay.isVisible())
  assert.deepEqual((await page.evaluate(() => window.fixtureImageCalls)).sort(), [[26, 2], [26, 7], [27, 0]])
  assert.deepEqual(await page.evaluate(() => window.fixtureTaskDetailCalls), [])
})

test('shows a download icon only on complete Skills with a safe name and preserves the exact ZIP contents', async (context) => {
  const name = 'fixture-injected'
  const body = '---\nname: fixture-injected\ndescription: 验证下载\n---\n\n# 检查\n保留正文与末尾换行。\n'
  const skillPath = '/project/.agents/skills/' + name + '/SKILL.md'
  const injected = '<skill>\n<name>' + name + '</name>\n<path>' + skillPath + '</path>\n' + body + '\n</skill>'
  const partial = '---\nname: fixture-partial\ndescription: 只保留元数据\n---'
  const truncated = '# 部分工具回执\n…500 chars truncated…'
  const unsafeName = '../fixture-unsafe'
  const unsafeBody = 'The Skill name is unsafe, but its recorded body remains readable.'
  const summary = { id: 26, started_at: record.created_at, request_ids: [26], user_preview: '验证 Skill 下载。' }
  const page = await reviewFixture(context, { ...record, text: '{}' }, undefined, {
    pages: { 0: { tasks: [summary], total_requests: 1, next_before: 0 } },
    tasks: { 26: {
      ...summary,
      steps: [],
      skills: [
        { kind: 'loaded_skill', state: 'received', name, source: 'tool_result',
          evidence: 'successful_read', path: skillPath, source_request_id: 25, text: '---\nname: fixture-injected\n---' },
        { kind: 'loaded_skill', state: 'received', name, source: 'user_message', evidence: 'injected_body',
          path: skillPath, source_request_id: 26, text: injected },
        { kind: 'loaded_skill', state: 'received', name: 'fixture-partial', source: 'tool_result',
          evidence: 'successful_read', path: '/project/.agents/skills/fixture-partial/SKILL.md', text: partial, source_request_id: 26 },
        { kind: 'loaded_skill', state: 'received', name: 'fixture-truncated', source: 'tool_result',
          evidence: 'successful_read', text: truncated, source_request_id: 26 },
        { kind: 'loaded_skill', state: 'received', name: unsafeName, source: 'tool_result',
          evidence: 'successful_read', text: unsafeBody, source_request_id: 26 },
      ],
    } },
  })
  await page.getByRole('button', { name: '查看会话' }).click()
  const replay = page.getByRole('dialog', { name: '任务回放' })
  await replay.getByRole('button', { name: '展开执行过程', exact: true }).click()
  const activity = replay.getByRole('heading', { name: 'Codex 做了什么' }).locator('..')
  const skillButton = (skillName) => activity.getByRole('button', { name: skillName, exact: true })
  const icon = skillButton(name).locator('svg.lucide-download')
  await icon.waitFor()
  assert.equal(await icon.getAttribute('aria-hidden'), 'true')
  const iconBox = await icon.boundingBox()
  assert.ok(iconBox)
  await page.mouse.click(iconBox.x + iconBox.width / 2, iconBox.y + iconBox.height / 2)
  const modal = page.getByRole('dialog', { name, exact: true })
  await modal.locator('pre').waitFor()
  assert.equal(await modal.locator('pre').textContent(), body)

  const [download] = await Promise.all([
    page.waitForEvent('download'),
    modal.getByRole('button', { name: '下载', exact: true }).click(),
  ])
  assert.equal(download.suggestedFilename(), name + '.zip')
  assert.equal(await download.failure(), null)
  const entries = readArchive(await readFile(await download.path()))
  assert.deepEqual(entries.filter(({ name }) => !name.endsWith('/')), [{ name: name + '/SKILL.md', body }])
  assert.ok(entries.filter(({ name }) => name.endsWith('/')).every((entry) => entry.name === name + '/'))
  await page.keyboard.press('Escape')
  await modal.waitFor({ state: 'hidden' })

  for (const [skillName, text] of [['fixture-partial', partial], ['fixture-truncated', truncated]]) {
    assert.equal(await skillButton(skillName).locator('svg.lucide-download').count(), 0)
    await skillButton(skillName).click()
    const partialModal = page.getByRole('dialog', { name: skillName, exact: true })
    await partialModal.getByText('仅保留了部分正文，无法下载完整的 SKILL.md', { exact: true }).waitFor()
    assert.equal(await partialModal.locator('pre').textContent(), text)
    assert.ok(await partialModal.getByRole('button', { name: '下载', exact: true }).isDisabled())
    await page.keyboard.press('Escape')
    await partialModal.waitFor({ state: 'hidden' })
  }

  assert.equal(await skillButton(unsafeName).locator('svg.lucide-download').count(), 0)
  await skillButton(unsafeName).click()
  const unsafeModal = page.getByRole('dialog', { name: unsafeName, exact: true })
  await unsafeModal.getByText('记录中的 Skill 名称无法用作下载文件名', { exact: true }).waitFor()
  assert.equal(await unsafeModal.locator('pre').textContent(), unsafeBody)
  assert.ok(await unsafeModal.getByRole('button', { name: '下载', exact: true }).isDisabled())
})

test('prepending earlier tasks preserves order and keeps missing or gateway-only results honest', async (context) => {
  const recent = { id: 26, started_at: record.created_at, request_ids: [26, 27], status: 'interrupted', user_text: '较新的任务。' }
  const earlier = { id: 10, started_at: '2026-11-21T01:00:00Z', request_ids: [10], status: 'completed', user_text: '较早的任务。',
    final_preview: '网关明确标记的内容。', final_status: 'completed', final_source: 'gateway', duration_ms: 35000, timing_source: 'gateway_observed' }
  const replay = {
    pages: {
      0: { tasks: [recent], total_requests: 3, next_before: 26 },
      26: { tasks: [earlier], total_requests: 3, next_before: 0 },
    },
    tasks: {
      26: { ...recent, user: { text: recent.user_text }, steps: [], skills: [] },
      10: {
        ...earlier, user: { text: earlier.user_text }, steps: [], skills: [],
        final: { text: earlier.final_preview, status: 'completed', source: 'gateway' },
      },
    },
  }
  const page = await reviewFixture(context, { ...record, text: '{}' }, undefined, replay)
  await page.getByRole('button', { name: '查看会话' }).click()
  const dialog = page.getByRole('dialog')
  await dialog.getByText('答复未记录', { exact: true }).waitFor()
  await dialog.getByText('整轮用时未记录', { exact: true }).waitFor()
  await dialog.getByText('已中断', { exact: true }).waitFor()
  await dialog.getByRole('button', { name: '加载更早的任务' }).click()
  const cards = dialog.getByRole('article')
  await cards.nth(1).waitFor()
  await cards.nth(0).getByText('较早的任务。', { exact: true }).waitFor()
  await cards.nth(1).getByText('较新的任务。', { exact: true }).waitFor()
  await dialog.getByText('网关捕获的最终回复', { exact: true }).waitFor()
  await dialog.getByText('网关观测区间 35 秒', { exact: true }).waitFor()
  assert.equal(await dialog.getByText('整轮用时 35 秒', { exact: true }).count(), 0)
  assert.deepEqual(await page.evaluate(() => window.fixtureTaskDetailCalls), [])
  const raw = dialog.locator('details').filter({ hasText: '逐条查看原始请求' })
  await raw.locator('summary').click()
  await raw.getByRole('button', { name: /#26/ }).click()
  await raw.getByRole('heading', { name: '完整请求文字' }).waitFor()
})

test('shows a completed Chat Completions answer in the card and pairs the model response with its request', async (context) => {
  const answer = '**可见答复**\n\n- 完成'
  const request = { ...record, id: 95, endpoint: '/v1/chat/completions' }
  const summary = {
    id: 95, started_at: request.created_at, request_ids: [95],
    status: 'completed', user_text: '能用什么 skills', final_preview: answer, final_status: 'completed', final_source: 'gateway',
    duration_ms: 7196, timing_source: 'gateway_observed',
  }
  const replay = {
    pages: { 0: { tasks: [summary], total_requests: 1, next_before: 0 } },
    tasks: { 95: {
      ...summary,
      user: { text: summary.user_text },
      steps: [{ request_id: 95, created_at: request.created_at, endpoint: request.endpoint, status: 200, tools: [],
        commentary: '可见答复', response: { text: answer, status: 'completed' } }],
      skills: [],
      final: { text: answer, status: 'completed', source: 'gateway' },
      duration_ms: 7196,
      timing_source: 'gateway_observed',
    } },
  }
  const page = await reviewFixture(context, {
    ...request,
    text: '{}',
    prompt_context: { sections: [] },
    result: { response: { text: answer, status: 'completed' } },
  }, undefined, replay)
  await page.getByRole('button', { name: '查看会话' }).click()
  const dialog = page.getByRole('dialog')
  const final = dialog.getByRole('heading', { name: '用户看到的最终答复' }).locator('..')
  await final.locator('strong').getByText('可见答复', { exact: true }).waitFor()
  await final.getByText('网关观测区间 7 秒').waitFor()
  assert.equal(await final.getByRole('button', { name: '展开全文', exact: true }).count(), 0)
  await dialog.getByRole('button', { name: '展开执行过程', exact: true }).click()
  await dialog.locator('summary').filter({ hasText: '#95 · 200' }).click()
  const response = dialog.getByRole('heading', { name: '对应的模型响应' }).locator('../..')
  await response.locator('strong').getByText('可见答复', { exact: true }).waitFor()
  assert.equal(await dialog.getByText('执行进展', { exact: true }).count(), 0)
  await dialog.getByRole('button', { name: '查看请求 #95' }).click()
  await dialog.getByRole('region', { name: '本轮提示词与 Skills' }).waitFor()
  assert.equal(await dialog.getByRole('heading', { name: '本轮结果' }).count(), 0)

  const raw = dialog.locator('details').filter({ hasText: '逐条查看原始请求' })
  await raw.locator('summary').click()
  await raw.getByRole('button', { name: /#95/ }).click()
  await raw.getByRole('heading', { name: '本轮结果' }).waitFor()
})

test('shows tools and loaded Skills up front while limiting execution steps to the first five', async (context) => {
  const firstSkill = { kind: 'loaded_skill', state: 'new', name: 'fixture-compile',
    evidence: 'successful_read', source: 'tool_result', source_request_id: 51 }
  const laterSkill = { kind: 'loaded_skill', state: 'new', name: 'fixture-verify',
    evidence: 'successful_read', source: 'tool_result', source_request_id: 56 }
  const requestIds = [49, 50, 51, 52, 53, 54, 55, 56]
  const summary = {
    id: 49, started_at: record.created_at, request_ids: requestIds, status: 'completed',
    user_preview: '检查故障。',
  }
  const steps = requestIds.map((id, index) => ({
    request_id: id, created_at: `2026-11-22T01:0${index}:00Z`,
    model: record.model, endpoint: record.endpoint, action: 'allow', status: 200,
    tools: id === 50 || id === 55 ? [{ name: 'exec_command', from_request_id: id + 1 }]
      : id === 56 ? [{ name: 'write_stdin' }] : [],
    ...(id === 50 ? { skills: [firstSkill] } : id === 55 ? { skills: [laterSkill] } : {}),
  }))
  const page = await reviewFixture(context, { ...record, id: 49, text: '{}' }, undefined, {
    pages: { 0: { tasks: [summary], total_requests: requestIds.length, next_before: 0 } },
    tasks: { 49: {
      ...summary, user: { text: '检查故障。' }, steps,
      skills: [firstSkill, laterSkill, { kind: 'attempted_skill', state: 'new', name: 'fixture-unconfirmed' }],
    } },
  })
  await page.getByRole('button', { name: '查看会话' }).click()
  const dialog = page.getByRole('dialog')
  await dialog.getByRole('button', { name: '展开执行过程', exact: true }).click()
  const activity = dialog.getByRole('heading', { name: 'Codex 做了什么' }).locator('..')
  const rows = activity.getByRole('list', { name: '执行步骤', exact: true }).locator('li > details > summary')
  const requestOrder = async () => (await rows.allTextContents()).map((row) => Number(row.match(/#(\d+)\s*·\s*200/)[1]))
  const verifyMarker = async (row, name, receipt) => {
    const marker = row.getByTitle('证据来自请求 #' + receipt).filter({ hasText: '已加载 · ' + name })
    await marker.waitFor()
    await marker.getByText('回执 #' + receipt, { exact: true }).waitFor()
    const markerBox = await marker.boundingBox()
    const rowBox = await row.boundingBox()
    assert.ok(markerBox && rowBox && rowBox.x + rowBox.width - (markerBox.x + markerBox.width) < 20)
  }

  await rows.first().waitFor()
  await activity.getByText('读取终端', { exact: true }).waitFor()
  await activity.getByText('fixture-verify', { exact: true }).first().waitFor()
  await activity.getByText('实际使用 · 未找到可核对的证据', { exact: true }).waitFor()
  assert.deepEqual(await requestOrder(), requestIds.slice(0, 5))
  await verifyMarker(rows.nth(1), 'fixture-compile', 51)
  assert.ok(!(await rows.nth(2).textContent()).includes('fixture-compile'))
  assert.equal(await rows.filter({ hasText: 'fixture-unconfirmed' }).count(), 0)

  await activity.getByRole('button', { name: '展开其余 3 条', exact: true }).click()
  assert.deepEqual(await requestOrder(), requestIds)
  await verifyMarker(rows.nth(6), 'fixture-verify', 56)
  await activity.getByRole('button', { name: '收起其余步骤', exact: true }).click()
  assert.deepEqual(await requestOrder(), requestIds.slice(0, 5))
  await activity.getByText('fixture-verify', { exact: true }).first().waitFor()
})

test('keeps confirmed Skill loading separate from a later step with usage evidence', async (context) => {
  const skillPath = '/project/.agents/skills/fixture-compile/SKILL.md'
  const helperPath = '/project/.agents/skills/fixture-compile/scripts/check.sh'
  const command = 'bash ' + helperPath
  const proof = 'Process exited with code 0'
  const loaded = { kind: 'loaded_skill', state: 'new', name: 'fixture-compile', path: skillPath,
    source: 'tool_result', evidence: 'successful_read', source_request_id: 51,
    text: 'Fixture instructions on checking a build.' }
  const attempted = { kind: 'attempted_skill', state: 'received', name: 'fixture-unconfirmed',
    path: '/project/.agents/skills/fixture-unconfirmed/SKILL.md',
    source: 'tool_result', evidence: 'truncated_body_unavailable', source_request_id: 54 }
  const unavailable = { ...attempted, name: 'fixture-unavailable',
    path: '/project/.agents/skills/fixture-unavailable/SKILL.md', evidence: 'body_unavailable', source_request_id: 52 }
  const usage = { name: loaded.name, path: helperPath, source: 'skill_helper_execution',
    source_request_id: 55, command, evidence: proof }
  const requestIds = [50, 51, 52, 53, 54, 55]
  const summary = {
    id: 50, started_at: record.created_at, request_ids: requestIds, status: 'completed',
    user_preview: '检查编译结果。',
  }
  const steps = requestIds.map((id, index) => ({
    request_id: id, created_at: `2026-11-22T01:0${index}:00Z`,
    model: record.model, endpoint: record.endpoint, action: 'allow', status: 200,
    tools: [50, 52, 53, 54].includes(id) ? [{ name: 'exec_command' }] : [],
    ...(id === 50 ? { skills: [loaded] } : {}),
    ...(id === 54 ? { skill_usage: [usage] } : {}),
  }))
  const page = await reviewFixture(context, { ...record, id: 50, text: '{}' }, undefined, {
    pages: { 0: { tasks: [summary], total_requests: requestIds.length, next_before: 0 } },
    tasks: { 50: { ...summary, user: { text: summary.user_preview }, steps, skills: [loaded, attempted, unavailable] } },
  })
  await page.getByRole('button', { name: '查看会话' }).click()
  const dialog = page.getByRole('dialog')
  await dialog.getByRole('button', { name: '展开执行过程', exact: true }).click()
  const activity = dialog.getByRole('heading', { name: 'Codex 做了什么' }).locator('..')
  const loadedOverview = activity.getByText('已加载 Skills', { exact: true }).locator('..')
  const usedOverview = activity.getByText('实际使用', { exact: true }).locator('..')
  await loadedOverview.getByText(loaded.name, { exact: true }).waitFor()
  await usedOverview.getByText(loaded.name, { exact: true }).waitFor()
  assert.equal(await usedOverview.getByText(attempted.name, { exact: true }).count(), 0)
  assert.equal(await usedOverview.getByText(unavailable.name, { exact: true }).count(), 0)

  const rows = activity.getByRole('list', { name: '执行步骤', exact: true }).locator('li > details')
  const loadRow = rows.filter({ hasText: '#50 · 200' })
  const useRow = rows.filter({ hasText: '#54 · 200' })
  const loadSummary = loadRow.locator(':scope > summary')
  const useSummary = useRow.locator(':scope > summary')
  await loadSummary.getByTitle('证据来自请求 #51').filter({ hasText: '已加载 · ' + loaded.name }).waitFor()
  await useSummary.getByTitle('证据来自请求 #55').filter({ hasText: '实际使用 · ' + loaded.name }).waitFor()
  assert.equal(await rows.filter({ hasText: '实际使用 · ' + loaded.name }).count(), 1)
  assert.equal(await rows.filter({ hasText: attempted.name }).count(), 0)
  assert.equal(await rows.filter({ hasText: unavailable.name }).count(), 0)
  assert.equal(await useRow.getByText(command, { exact: true }).isVisible(), false)

  await useSummary.click()
  await useRow.getByText('使用依据', { exact: true }).waitFor()
  await useRow.getByText(command, { exact: true }).waitFor()
  await useRow.getByText(proof, { exact: true }).waitFor()
  assert.equal(await useRow.getByText('加载依据', { exact: true }).count(), 0)
  await loadSummary.click()
  await loadRow.getByText('加载依据', { exact: true }).waitFor()
  await loadRow.getByText(skillPath, { exact: true }).waitFor()
  assert.equal(await loadRow.getByText('使用依据', { exact: true }).count(), 0)

  const attempts = activity.locator('summary').filter({ hasText: '尝试读取 Skill' })
  await attempts.click()
  const attemptCard = (name) => attempts.locator('..').locator('strong').filter({ hasText: name }).locator('..').locator('..')
  const truncatedAttempt = attemptCard(attempted.name)
  const unavailableAttempt = attemptCard(unavailable.name)
  await truncatedAttempt.getByText('证据：回执已截断，保留内容未包含可归属的 Skill 正文', { exact: true }).waitFor()
  await unavailableAttempt.getByText('证据：此次回执中未取得可归属的 Skill 正文', { exact: true }).waitFor()
})

test('limits wrapped user content and a Markdown reply to five visible lines independently', async (context) => {
  const userText = 'G:\\Workspace\\Project\\Source\\Construction\\ProductionSubsystem.Gather.cpp\\'.repeat(20)
  const code = Array.from({ length: 8 }, (_, index) => `fixture output ${index + 1}`).join('\n')
  const replyText = '**格式化回复**\n\n- 排查编译错误\n\n```text\n' + code + '\n```\n\n[编译报告](https://example.test/report)'
  const summary = {
    id: 26, started_at: record.created_at, request_ids: [26], status: 'completed',
    user_text: userText, final_preview: replyText, final_status: 'completed', final_source: 'client',
    duration_ms: 560000, timing_source: 'client',
  }
  const page = await reviewFixture(context, { ...record, text: '{}' }, undefined, {
    pages: { 0: { tasks: [summary], total_requests: 1, next_before: 0 } },
    tasks: { 26: {
      ...summary,
      user: { text: userText },
      skills: [],
      steps: [],
      final: { text: replyText, status: 'completed', source: 'client' },
      duration_ms: 560000,
      timing_source: 'client',
    } },
  })
  await page.setViewportSize({ width: 430, height: 720 })
  await page.evaluate(() => document.documentElement.classList.add('dark'))
  await page.getByRole('button', { name: '查看会话' }).click()
  const user = page.getByRole('heading', { name: '用户发送了什么' }).locator('..')
  const reply = page.getByRole('heading', { name: '用户看到的最终答复' }).locator('..')
  const copy = user.getByText(userText, { exact: true })
  const reportLink = reply.locator('a[href="https://example.test/report"]')
  const expansion = async (section) => {
    const expand = section.getByRole('button', { name: '展开全文', exact: true })
    await expand.waitFor()
    const id = await expand.getAttribute('aria-controls')
    assert.ok(id)
    const viewport = page.locator(`[id=${JSON.stringify(id)}]`)
    return {
      expand,
      collapse: section.getByRole('button', { name: '收起', exact: true }),
      visibleLines: () => viewport.evaluate((element) => {
        const style = getComputedStyle(element)
        const verticalInsets = [style.paddingTop, style.paddingBottom, style.borderTopWidth, style.borderBottomWidth]
          .reduce((sum, value) => sum + Number.parseFloat(value), 0)
        return (element.getBoundingClientRect().height - verticalInsets) / Number.parseFloat(style.lineHeight)
      }),
    }
  }
  const userTextDisclosure = await expansion(user)
  const replyDisclosure = await expansion(reply)
  const expectDisclosureButton = async (button, expanded) => {
    assert.equal(await button.getAttribute('aria-expanded'), String(expanded))
    const appearance = await button.evaluate((element) => {
      const style = getComputedStyle(element)
      return {
        borderWidth: Number.parseFloat(style.borderTopWidth),
        borderStyle: style.borderTopStyle,
        borderColor: style.borderTopColor,
        backgroundColor: style.backgroundColor,
        cursor: style.cursor,
        height: element.getBoundingClientRect().height,
      }
    })
    assert.ok(appearance.borderWidth > 0)
    assert.equal(appearance.borderStyle, 'solid')
    assert.notEqual(appearance.borderColor, 'rgba(0, 0, 0, 0)')
    assert.notEqual(appearance.borderColor, appearance.backgroundColor)
    assert.equal(appearance.cursor, 'pointer')
    assert.ok(appearance.height > 0 && appearance.height <= 32)
    const icon = button.locator('svg.lucide-chevron-down')
    assert.ok(await icon.isVisible())
    assert.equal(await icon.getAttribute('aria-hidden'), 'true')
    assert.equal(await icon.evaluate((element) => getComputedStyle(element).rotate), expanded ? '180deg' : 'none')
  }
  const expectCollapsed = async ({ expand, visibleLines }) => {
    await expectDisclosureButton(expand, false)
    assert.ok(Math.abs(await visibleLines() - 5) < 0.05)
  }
  await expectCollapsed(userTextDisclosure)
  await expectCollapsed(replyDisclosure)
  assert.equal(await reportLink.evaluate((link) => link.inert), true)
  await reply.getByText('整轮用时 9 分钟 20 秒').waitFor()
  await reply.getByText('客户端记录', { exact: true }).waitFor()

  await userTextDisclosure.expand.click()
  await expectDisclosureButton(userTextDisclosure.collapse, true)
  assert.ok(await userTextDisclosure.visibleLines() > 5)
  assert.equal(await copy.textContent(), userText)
  await expectCollapsed(replyDisclosure)

  await replyDisclosure.expand.click()
  await expectDisclosureButton(replyDisclosure.collapse, true)
  assert.ok(await replyDisclosure.visibleLines() > 5)
  assert.equal(await reportLink.evaluate((link) => link.inert), false)
  await reply.locator('strong').getByText('格式化回复', { exact: true }).waitFor()
  assert.equal(await reply.locator('pre code').textContent(), code + '\n')
  await userTextDisclosure.collapse.click()
  await expectCollapsed(userTextDisclosure)
  assert.equal(await replyDisclosure.collapse.getAttribute('aria-expanded'), 'true')
  await replyDisclosure.collapse.click()
  await expectCollapsed(replyDisclosure)
  assert.equal(await reportLink.evaluate((link) => link.inert), true)
  await reply.getByText('整轮用时 9 分钟 20 秒').waitFor()
})

test('right-aligns user bubbles and shows context compaction in execution order before the reply', async (context) => {
  const time = (seconds) => new Date(Date.parse(record.created_at) + seconds * 1000).toISOString()
  const event = (id, at) => ({ id, started_at: at, request_ids: [id], kind: 'context_compaction' })
  const longerInput = '可以，继续做吧。请具体列出接下来要做的工作。'.repeat(7)
  const tasks = [
    { id: 159, started_at: record.created_at, request_ids: [159], request_count: 29, user_text: '继续',
      final_preview: '**已完成核对。**', final_status: 'completed', final_source: 'client', duration_ms: 93000, timing_source: 'client' },
    { id: 188, started_at: time(100), request_ids: [188], request_count: 36, user_text: longerInput, status: 'interrupted' },
    { id: 433, started_at: time(500), request_ids: [433], request_count: 112, user_text: '继续', status: 'interrupted',
      final_preview: '已保存此前的确认结果。', final_status: 'completed', final_source: 'gateway' },
  ]
  const internalText = 'You are performing a CONTEXT CHECKPOINT COMPACTION. Create a handoff summary for another LLM.'
  const page = await reviewFixture(context, { ...record, text: '{}' }, undefined, {
    pages: { 0: { tasks, total_requests: 281, next_before: 0,
      internal_events: [event(432, time(490)), event(442, time(520)), event(422, time(450))] } },
    tasks: { 433: { ...tasks[2], skills: [], steps: [433, 434, 435, 436].map((id) => ({
      request_id: id, created_at: time(500 + id - 433), endpoint: record.endpoint, model: record.model, status: 200, tools: [],
    })) } },
    requests: Object.fromEntries([422, 432].map((id) => [id, { ...record, id, text: JSON.stringify({ input: internalText }) }])),
  })
  await page.getByRole('button', { name: '查看会话' }).click()
  const dialog = page.getByRole('dialog')
  const cards = dialog.getByRole('article')
  await dialog.getByText('共 281 次模型调用', { exact: true }).waitFor()
  const continued = dialog.getByRole('article', { name: '任务 #159', exact: true })
  await continued.getByText('继续', { exact: true }).waitFor()
  await continued.getByText('共 29 次模型调用', { exact: true }).waitFor()
  await continued.locator('strong').getByText('已完成核对。', { exact: true }).waitFor()
  await continued.getByText('整轮用时 1 分钟 33 秒', { exact: true }).waitFor()
  const interrupted = dialog.getByRole('article', { name: '任务 #188', exact: true })
  await interrupted.getByText('答复未记录', { exact: true }).waitFor()
  await interrupted.getByText('已中断', { exact: true }).waitFor()
  const interruptedWithReply = dialog.getByRole('article', { name: '任务 #433', exact: true })
  await interruptedWithReply.getByText('已中断', { exact: true }).waitFor()
  await interruptedWithReply.getByText('已保存此前的确认结果。', { exact: true }).waitFor()
  assert.deepEqual(await cards.locator('h3').allTextContents(), ['任务 #159', '任务 #188', '任务 #433'])
  assert.equal(await dialog.getByText(internalText).count(), 0)
  assert.deepEqual(await page.evaluate(() => window.fixtureTaskDetailCalls), [])

  const user = (card) => card.getByRole('region', { name: '用户发送了什么', exact: true })
  const bubble = (card) => user(card).locator(':scope > div').first()
  const measureBubble = (card) => bubble(card).evaluate((element) => {
    const bounds = element.getBoundingClientRect()
    const parent = element.parentElement.getBoundingClientRect()
    return { width: bounds.width, available: parent.width, rightGap: parent.right - bounds.right }
  })
  const narrow = await measureBubble(continued)
  const long = await measureBubble(interrupted)
  assert.ok(Math.abs(narrow.rightGap) < 2 && Math.abs(long.rightGap) < 2)
  assert.ok(narrow.width < long.width && narrow.width < narrow.available / 2)
  await page.setViewportSize({ width: 390, height: 850 })
  const mobile = await measureBubble(interrupted)
  assert.ok(Math.abs(mobile.rightGap) < 2 && mobile.width <= mobile.available + 1)

  assert.equal(await dialog.locator('summary').filter({ hasText: '内部事件' }).count(), 0)
  const execute = interruptedWithReply.getByRole('button', { name: '展开执行过程', exact: true })
  const reply = interruptedWithReply.getByRole('heading', { name: '用户看到的最终答复', exact: true })
  const userBox = await user(interruptedWithReply).boundingBox()
  const executeBox = await execute.boundingBox()
  const replyBox = await reply.boundingBox()
  assert.ok(userBox.y < executeBox.y && executeBox.y < replyBox.y)
  const compaction = (id) => interruptedWithReply.getByRole('button', { name: new RegExp(`上下文压缩 · #${id}`) })
  assert.equal(await compaction(422).count() + await compaction(432).count(), 0)
  await execute.click()
  const steps = interruptedWithReply.getByRole('list', { name: '执行步骤' })
  await steps.waitFor()
  assert.deepEqual(await page.evaluate(() => window.fixtureTaskDetailCalls), [433])
  const rows = steps.locator(':scope > li')
  assert.equal(await rows.count(), 5)
  assert.deepEqual((await rows.allTextContents()).map((row) => row.match(/#(\d+)/)?.[1]), ['422', '432', '433', '434', '435'])
  assert.ok((await steps.boundingBox()).y < (await reply.boundingBox()).y)
  for (const id of [422, 432]) {
    await compaction(id).click()
    await interruptedWithReply.getByText(internalText).waitFor()
    assert.equal(await compaction(id).getAttribute('aria-expanded'), 'true')
    await compaction(id).click()
    assert.equal(await compaction(id).getAttribute('aria-expanded'), 'false')
  }
  await interruptedWithReply.getByRole('button', { name: '收起执行过程', exact: true }).click()
  await steps.waitFor({ state: 'hidden' })
  await interruptedWithReply.getByText('已保存此前的确认结果。', { exact: true }).waitFor()

  // If the recording ends at compaction, the event stays inspectable without
  // fabricating a new user submission or reopening the preceding task.
  const trailingExecution = dialog.locator('summary').filter({ hasText: /^执行过程$/ })
  assert.equal(await dialog.getByRole('article').count(), 3)
  assert.equal(await interruptedWithReply.getByRole('button', { name: /上下文压缩 · #442/ }).count(), 0)
  await trailingExecution.click()
  await trailingExecution.locator('..').getByRole('button', { name: /上下文压缩 · #442/ }).waitFor()
  assert.deepEqual(await page.evaluate(() => window.fixtureTaskDetailCalls), [433])
})

test('fetches the full original and answer only when a capped card preview is expanded', async (context) => {
  const fullUser = '用户原文\n' + '后续行\n'.repeat(8) + '完整原文末尾'
  const fullFinal = '**答复预览**\n\n' + '后续文字\n\n'.repeat(8) + '完整答复末尾'
  const summary = { id: 26, started_at: record.created_at, request_ids: [26], user_text: '用户原文', user_text_truncated: true,
    status: 'completed', final_preview: '**答复预览**', final_status: 'completed', final_source: 'gateway', final_preview_truncated: true }
  const page = await reviewFixture(context, { ...record, text: '{}' }, undefined, {
    pages: { 0: { tasks: [summary], total_requests: 1, next_before: 0 } },
    tasks: { 26: { ...summary, user: { text: fullUser }, final: { text: fullFinal, status: 'completed', source: 'gateway' }, steps: [], skills: [] } },
  })
  await page.getByRole('button', { name: '查看会话' }).click()
  const card = page.getByRole('article')
  const user = card.getByRole('heading', { name: '用户发送了什么' }).locator('..')
  const reply = card.getByRole('heading', { name: '用户看到的最终答复' }).locator('..')
  await user.getByText('用户原文', { exact: true }).waitFor()
  await reply.locator('strong').getByText('答复预览', { exact: true }).waitFor()
  assert.deepEqual(await page.evaluate(() => window.fixtureTaskDetailCalls), [])
  await user.getByRole('button', { name: '展开全文', exact: true }).click()
  await user.getByRole('button', { name: '收起', exact: true }).waitFor()
  assert.equal(await user.getByText(/完整原文末尾/).textContent(), fullUser)
  await reply.getByRole('button', { name: '展开全文', exact: true }).click()
  await reply.getByRole('button', { name: '收起', exact: true }).waitFor()
  await reply.getByText('完整答复末尾', { exact: true }).waitFor()
  assert.deepEqual(await page.evaluate(() => window.fixtureTaskDetailCalls), [26])
  assert.equal(await card.getByRole('button', { name: '展开执行过程' }).getAttribute('aria-expanded'), 'false')
})
