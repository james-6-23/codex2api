import assert from 'node:assert/strict'
import { readFileSync } from 'node:fs'
import test from 'node:test'

import {
  hasBlockingModalOrOverlay,
  isEditableKeyboardTarget,
  shouldHandleAccountDetailKeyNavigation,
} from './accountDetailSheetKeyboard.ts'

const detailSheetSource = readFileSync(
  new URL('../components/AccountDetailSheet.tsx', import.meta.url),
  'utf8',
)
const keyboardHelperSource = readFileSync(
  new URL('./accountDetailSheetKeyboard.ts', import.meta.url),
  'utf8',
)

/**
 * Creates a mock document for querying overlays.
 *
 * @param {object} options
 * @param {string[]} [options.matchingSelectors]
 * @param {number} [options.sheetCount]
 * @returns {object}
 */
function createMockDocument({ matchingSelectors = [], sheetCount = 1 } = {}) {
  return {
    querySelector: (selector) => {
      const match = matchingSelectors.some((sel) => selector.includes(sel))
      return match ? {} : null
    },
    querySelectorAll: (selector) => {
      if (selector.includes('[data-slot="sheet-content"]')) {
        return { length: sheetCount }
      }
      return { length: 0 }
    },
  }
}

/**
 * Creates a mock DOM element with a closest() matcher.
 *
 * @param {string[]} matchingTags
 * @returns {object}
 */
function createMockElement(matchingTags = []) {
  return {
    closest: (selector) => {
      const match = matchingTags.some((tag) => selector.includes(tag))
      return match ? {} : null
    },
  }
}

test('isEditableKeyboardTarget identifies inputs, textareas, selects, and contenteditable', () => {
  assert.equal(isEditableKeyboardTarget(createMockElement(['input'])), true)
  assert.equal(isEditableKeyboardTarget(createMockElement(['textarea'])), true)
  assert.equal(isEditableKeyboardTarget(createMockElement(['select'])), true)
  assert.equal(isEditableKeyboardTarget(createMockElement(['contenteditable'])), true)
  assert.equal(isEditableKeyboardTarget(createMockElement([])), false)
  assert.equal(isEditableKeyboardTarget(null), false)
  assert.equal(isEditableKeyboardTarget({}), false)
})

test('hasBlockingModalOrOverlay recognizes dialogs, alert dialogs, dropdowns, and multiple sheets', () => {
  const dialogDoc = createMockDocument({ matchingSelectors: ['[data-slot="dialog-content"]'] })
  assert.equal(hasBlockingModalOrOverlay(dialogDoc), true)

  const alertDoc = createMockDocument({ matchingSelectors: ['[role="alertdialog"]'] })
  assert.equal(hasBlockingModalOrOverlay(alertDoc), true)

  const selectDoc = createMockDocument({ matchingSelectors: ['[data-select-dropdown="true"]'] })
  assert.equal(hasBlockingModalOrOverlay(selectDoc), true)

  const multiSheetDoc = createMockDocument({ sheetCount: 2 })
  assert.equal(hasBlockingModalOrOverlay(multiSheetDoc), true)

  const singleSheetDoc = createMockDocument({ sheetCount: 1 })
  assert.equal(hasBlockingModalOrOverlay(singleSheetDoc), false)

  assert.equal(hasBlockingModalOrOverlay(null), false)
})

test('shouldHandleAccountDetailKeyNavigation gates keyboard navigation', () => {
  const normalDoc = createMockDocument({ sheetCount: 1 })
  const plainTarget = createMockElement([])
  const inputTarget = createMockElement(['input'])
  const blockingDoc = createMockDocument({ matchingSelectors: ['[data-slot="dialog-content"]'] })
  const multiSheetDoc = createMockDocument({ sheetCount: 2 })

  assert.equal(
    shouldHandleAccountDetailKeyNavigation({ key: 'ArrowLeft', target: plainTarget }, normalDoc),
    true,
  )
  assert.equal(
    shouldHandleAccountDetailKeyNavigation({ key: 'ArrowRight', target: plainTarget }, normalDoc),
    true,
  )

  assert.equal(
    shouldHandleAccountDetailKeyNavigation({ key: 'Enter', target: plainTarget }, normalDoc),
    false,
  )

  assert.equal(
    shouldHandleAccountDetailKeyNavigation(
      { key: 'ArrowLeft', shiftKey: true, target: plainTarget },
      normalDoc,
    ),
    false,
  )
  assert.equal(
    shouldHandleAccountDetailKeyNavigation(
      { key: 'ArrowLeft', ctrlKey: true, target: plainTarget },
      normalDoc,
    ),
    false,
  )
  assert.equal(
    shouldHandleAccountDetailKeyNavigation(
      { key: 'ArrowRight', altKey: true, target: plainTarget },
      normalDoc,
    ),
    false,
  )
  assert.equal(
    shouldHandleAccountDetailKeyNavigation(
      { key: 'ArrowRight', metaKey: true, target: plainTarget },
      normalDoc,
    ),
    false,
  )

  assert.equal(
    shouldHandleAccountDetailKeyNavigation({ key: 'ArrowLeft', target: inputTarget }, normalDoc),
    false,
  )
  assert.equal(
    shouldHandleAccountDetailKeyNavigation({ key: 'ArrowRight', target: inputTarget }, normalDoc),
    false,
  )

  assert.equal(
    shouldHandleAccountDetailKeyNavigation({ key: 'ArrowLeft', target: plainTarget }, blockingDoc),
    false,
  )
  assert.equal(
    shouldHandleAccountDetailKeyNavigation({ key: 'ArrowRight', target: plainTarget }, multiSheetDoc),
    false,
  )
})

test('AccountDetailSheet integrates keyboard navigation guard', () => {
  assert.match(
    detailSheetSource,
    /shouldHandleAccountDetailKeyNavigation\(\s*event\s*\)/,
  )
  assert.match(
    detailSheetSource,
    /window\.addEventListener\(\s*['"]keydown['"]\s*,\s*onKeyDown\s*\)/,
  )
})

test('accountDetailSheetKeyboard source declares required overlay and sheet count selectors', () => {
  assert.match(keyboardHelperSource, /\[data-slot="dialog-content"\]/)
  assert.match(
    keyboardHelperSource,
    /querySelectorAll\(\s*['"]\[data-slot="sheet-content"\]['"]\s*\)\.length\s*>\s*1/,
  )
})
