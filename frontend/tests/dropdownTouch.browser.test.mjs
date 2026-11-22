import assert from 'node:assert/strict'
import { after, before, test } from 'node:test'
import { fileURLToPath } from 'node:url'
import { build } from 'vite'
import tailwindcss from '@tailwindcss/vite'
import { chromium } from 'playwright'

// Exercise browser-generated touch/click events on real options, including a
// body portal outside a scroll-locked Radix Dialog. Setting scrollTop directly
// would miss the regression where touching an option unmounts the list.
const root = fileURLToPath(new URL('../', import.meta.url))
let browser, js, css
before(async () => {
  const bundle = await build({
    configFile: false, root, logLevel: 'silent',
    resolve: { alias: { '@': root + 'src' } },
    define: { 'process.env.NODE_ENV': '"production"' },
    plugins: [tailwindcss(), {
      name: 'touch-harness', enforce: 'pre',
      resolveId(id) { if (id === 'virtual:dropdown') return '\0dropdown' },
      load(id) {
        if (id !== '\0dropdown') return
        return `import {createElement as h} from 'react'; import {createRoot} from 'react-dom/client';
          import {Select} from '@/components/ui/select'; import Chip from '@/components/ChipInput';
          import {AutoRefreshButton} from '@/components/AutoRefreshButton';
          import {Dialog,DialogContent,DialogTitle} from '@/components/ui/dialog'; import '@/index.css';
          const options = Array.from({length:80},(_,i)=>'option '+i);
          window.picks=[]; const pick=v=>window.picks.push(v);
          const picker = window.kind === 'select' ? h(Select,{value:'',options:options.map(value=>({value,label:value})),onValueChange:pick}) : h(Chip,{value:[],options,onChange:pick});
          createRoot(document.querySelector('#root')).render(window.kind === 'refresh' ? h(AutoRefreshButton,{intervals:options.map((_,i)=>i),onRefresh:()=>{},onAutoRefresh:async()=>{},saveSeconds:pick}) : h(Dialog,{open:true},h(DialogContent,null,h(DialogTitle,null,'Options'),picker)));`
      },
    }],
    build: { write: false, rolldownOptions: { input: 'virtual:dropdown' }, lib: { entry: 'virtual:dropdown', formats: ['iife'], name: 'TouchHarness' } },
  })
  const { output } = Array.isArray(bundle) ? bundle[0] : bundle
  js = output.find(f => f.type === 'chunk').code
  css = output.find(f => f.type === 'asset' && f.fileName.endsWith('.css')).source
  browser = await chromium.launch({ executablePath: process.env.PLAYWRIGHT_CHROMIUM_EXECUTABLE_PATH })
})
after(() => browser?.close())

for (const kind of ['select', 'chip', 'refresh']) {
  test(`${kind}: swipe scrolls without selecting, subsequent tap picks once`, async t => {
    const page = await browser.newPage({ isMobile: true, hasTouch: true, viewport: { width: 390, height: 750 } })
    t.after(() => page.close())
    await page.setContent('<meta name="viewport" content="width=device-width, initial-scale=1"><div id="root"></div>')
    await page.evaluate(k => { window.kind = k }, kind)
    await page.addStyleTag({ content: css }); await page.addScriptTag({ content: js })
    if (kind === 'select') await page.locator('[data-slot="select-trigger"]').tap()
    else if (kind === 'refresh') await page.locator('button[aria-haspopup="menu"]').tap()
    else await page.getByRole('textbox').tap()
    const items = page.locator(kind === 'refresh' ? '[role="menuitemradio"]' : '[role="option"]')
    const item = items.nth(3)
    await item.waitFor()
    const rect = await item.boundingBox()
    const x = rect.x + rect.width / 2, y = rect.y + rect.height / 2
    const cdp = await page.context().newCDPSession(page)
    for (const [type, dy] of [['touchStart', 0], ['touchMove', 30], ['touchMove', 65], ['touchEnd', null]]) {
      await cdp.send('Input.dispatchTouchEvent', { type, touchPoints: dy === null ? [] : [{ x, y: y - dy }] })
    }
    const state = await page.evaluate(() => {
      const list = document.querySelector('[role="listbox"], [role="menu"]')
      return { picks: window.picks, top: Math.max(list?.scrollTop ?? 0, list?.parentElement.scrollTop ?? 0) }
    })
    assert.deepEqual(state.picks, [])
    assert.ok(state.top > 0, 'list must stay open and actually scroll')
    await items.nth(5).tap()
    assert.deepEqual(await page.evaluate(() => window.picks), kind === 'refresh' ? [5] : kind === 'select' ? ['option 5'] : [['option 5']])
  })
}
