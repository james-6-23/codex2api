import assert from 'node:assert/strict'
import { readdirSync, readFileSync, statSync } from 'node:fs'
import { join, relative } from 'node:path'
import { fileURLToPath } from 'node:url'
import test from 'node:test'

const srcRoot = fileURLToPath(new URL('..', import.meta.url))

function walk(dir, out = []) {
  for (const name of readdirSync(dir)) {
    const full = join(dir, name)
    if (statSync(full).isDirectory()) walk(full, out)
    else if (full.endsWith('.tsx')) out.push(full)
  }
  return out
}

const SHARED_SELECT = join(srcRoot, 'components', 'ui', 'select.tsx')

test('pages and components use the shared Select instead of a raw <select>', () => {
  const files = [...walk(join(srcRoot, 'pages')), ...walk(join(srcRoot, 'components'))].filter((f) => f !== SHARED_SELECT)
  const offenders = files.filter((f) => /<select[\s>]/.test(readFileSync(f, 'utf8'))).map((f) => relative(srcRoot, f))
  assert.deepEqual(offenders, [], `raw <select> found; use components/ui/select.tsx (see DESIGN.md): ${offenders.join(', ')}`)
})

test('Proxies risk selects stay content-sized (not the Select wrapper default w-full)', () => {
  const proxiesPath = join(srcRoot, 'pages', 'Proxies.tsx')
  const content = readFileSync(proxiesPath, 'utf8')
  const selectBlocks = content.match(/<Select\b[\s\S]*?\/>/g) ?? []
  assert.equal(selectBlocks.length, 2, `expected 2 <Select /> usages in Proxies.tsx, found ${selectBlocks.length}`)
  for (const block of selectBlocks) {
    assert.match(
      block,
      /className=["'][^"']*\bw-auto\b[^"']*["']/,
      `Select wrapper defaults to w-full unless className overrides it with w-auto; missing on: ${block.slice(0, 60)}...`
    )
  }
})
