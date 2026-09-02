import assert from 'node:assert/strict'

// Extract a declaration body without depending on LF/CRLF or the exact spacing
// between the declaration and the following render/function boundary.
export function extractBalancedBody(source, marker, message = marker) {
  const markerIndex = source.indexOf(marker)
  assert.ok(markerIndex >= 0, `missing ${message}`)
  const open = source.indexOf('{', markerIndex + marker.length)
  assert.ok(open >= 0, `missing opening brace for ${message}`)

  let depth = 0
  let quote = null
  let lineComment = false
  let blockComment = false
  let escaped = false
  for (let index = open; index < source.length; index += 1) {
    const char = source[index]
    const next = source[index + 1]
    if (lineComment) {
      if (char === '\n') lineComment = false
      continue
    }
    if (blockComment) {
      if (char === '*' && next === '/') {
        blockComment = false
        index += 1
      }
      continue
    }
    if (quote !== null) {
      if (escaped) {
        escaped = false
      } else if (char === '\\') {
        escaped = true
      } else if (char === quote) {
        quote = null
      }
      continue
    }
    if (char === '/' && next === '/') {
      lineComment = true
      index += 1
      continue
    }
    if (char === '/' && next === '*') {
      blockComment = true
      index += 1
      continue
    }
    if (char === "'" || char === '"' || char === '`') {
      quote = char
      continue
    }
    if (char === '{') depth += 1
    if (char === '}') {
      depth -= 1
      if (depth === 0) return source.slice(markerIndex, index + 1)
    }
  }
  assert.fail(`missing closing brace for ${message}`)
}
