import assert from 'node:assert/strict'
import { readFileSync } from 'node:fs'
import test from 'node:test'
import { runInNewContext } from 'node:vm'

const html = readFileSync(new URL('../../index.html', import.meta.url), 'utf8')
const bootstrap = html.match(/<script>\s*([\s\S]*?)\s*<\/script>/)?.[1]
assert.ok(bootstrap, 'the document must initialize its theme before React loads')

function runBootstrap({ mode, colorTheme, systemDark = false, storageError = false, accessError = false } = {}) {
  const classes = new Set()
  const context = {
    document: { documentElement: { classList: { add: (value) => classes.add(value) } } },
    matchMedia: (query) => {
      assert.equal(query, '(prefers-color-scheme: dark)')
      return { matches: systemDark }
    },
  }
  Object.defineProperty(context, 'localStorage', {
    get() {
      if (accessError) throw new Error('Storage access denied')
      return {
        getItem(key) {
          if (storageError) throw new Error('Storage read denied')
          return (key === 'theme' ? mode : colorTheme) ?? null
        },
      }
    },
  })
  runInNewContext(bootstrap, context)
  return classes
}

test('all eight new palettes apply on the first render in both appearances', () => {
  for (const colorTheme of ['ocean', 'forest', 'lavender', 'amber', 'espresso', 'sakura', 'mint', 'midnight']) {
    for (const mode of ['light', 'dark']) {
      const classes = runBootstrap({ colorTheme, mode })
      assert.ok(classes.has(`theme-${colorTheme}`), `${colorTheme}: ${mode}`)
      assert.equal(classes.has('dark'), mode === 'dark')
      assert.equal(classes.has('theme-studio'), false)
    }
  }
})

test('saved website palettes apply before React loads in light and dark modes', () => {
  for (const colorTheme of ['github', 'vercel', 'google', 'apple', 'notion', 'stripe', 'spotify', 'slack']) {
    for (const mode of ['light', 'dark']) {
      for (const systemDark of [false, true]) {
        const classes = runBootstrap({ colorTheme, mode, systemDark })
        assert.deepEqual(
          [...classes],
          mode === 'dark' ? ['dark', `theme-${colorTheme}`] : [`theme-${colorTheme}`],
          `${colorTheme}: saved ${mode} mode, system dark ${systemDark}`,
        )
      }
    }
  }
})

test('saved website palettes survive a reload when the system appearance changes', () => {
  for (const colorTheme of ['github', 'vercel', 'google', 'apple', 'notion', 'stripe', 'spotify', 'slack']) {
    const savedPreference = { colorTheme, mode: 'system' }
    assert.deepEqual([...runBootstrap({ ...savedPreference, systemDark: false })], [`theme-${colorTheme}`])
    assert.deepEqual([...runBootstrap({ ...savedPreference, systemDark: true })], ['dark', `theme-${colorTheme}`])
  }
})

test('saved system mode and missing or invalid mode follow the device appearance', () => {
  for (const mode of ['system', undefined, '', 'unknown']) {
    for (const systemDark of [false, true]) {
      const classes = runBootstrap({ mode, systemDark, colorTheme: 'midnight' })
      assert.equal(classes.has('dark'), systemDark, `${String(mode)}: ${systemDark}`)
      assert.ok(classes.has('theme-midnight'))
    }
  }
})

test('explicit light and dark modes override the device appearance', () => {
  assert.equal(runBootstrap({ mode: 'light', systemDark: true }).has('dark'), false)
  assert.equal(runBootstrap({ mode: 'dark', systemDark: false }).has('dark'), true)
})

test('existing saved palettes remain supported', () => {
  for (const colorTheme of [
    'studio', 'default', 'claude', 'chatgpt', 'deepseek', 'graphite', 'aurora', 'rose', 'mono',
    'one-dark-pro', 'github-dimmed', 'tokyo-night', 'dracula', 'monokai-pro',
    'nord', 'catppuccin', 'gruvbox', 'solarized-light', 'quiet-light', 'ayu-light', 'noctis-lux',
  ]) {
    assert.ok(runBootstrap({ colorTheme }).has(`theme-${colorTheme}`), colorTheme)
  }
})

test('missing and unknown palettes fall back to the studio default without adding stored text as a class', () => {
  for (const colorTheme of [undefined, '', 'unknown', 'ocean extra-class', 'OCEAN']) {
    assert.deepEqual([...runBootstrap({ colorTheme, mode: 'light' })], ['theme-studio'])
  }
})

test('unavailable storage still initializes the studio default and follows the device', () => {
  for (const failure of [{ storageError: true }, { accessError: true }]) {
    for (const systemDark of [false, true]) {
      const classes = runBootstrap({ ...failure, systemDark })
      assert.ok(classes.has('theme-studio'))
      assert.equal(classes.has('dark'), systemDark)
    }
  }
})
