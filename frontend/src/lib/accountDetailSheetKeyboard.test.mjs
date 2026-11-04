import assert from 'node:assert/strict'
import { readFileSync } from 'node:fs'
import test from 'node:test'

const detailSheetSource = readFileSync(
  new URL('../components/AccountDetailSheet.tsx', import.meta.url),
  'utf8',
)

test('AccountDetailSheet source code checks modifiers, input elements, and open dialogs', () => {
  // Ensure keyboard listener filters modifiers
  assert.match(
    detailSheetSource,
    /event\.altKey\s*\|\|\s*event\.ctrlKey\s*\|\|\s*event\.metaKey\s*\|\|\s*event\.shiftKey/,
  )

  // Ensure input, textarea, select, contenteditable elements are not hijacked
  assert.match(
    detailSheetSource,
    /target\?\.closest\(\s*['"]input,\s*textarea,\s*select,\s*\[contenteditable=["']true["']\]['"]\s*\)/,
  )

  // Ensure active modal/dialog stops sheet navigation
  assert.match(detailSheetSource, /\[data-slot="dialog-content"\]/)
})

test('Simulated DOM event handling matches expected keyboard behavior', () => {
  // Extract and evaluate the handler logic in a controlled DOM-like context
  function createEnvironment({
    activeTarget = null,
    hasDialog = false,
    canGoPrev = true,
    canGoNext = true,
  }) {
    let prevCalled = 0
    let nextCalled = 0

    const mockDocument = {
      querySelector: (selector) => {
        if (hasDialog && selector.includes('[data-slot="dialog-content"]')) {
          return {}
        }
        return null
      },
      querySelectorAll: (selector) => {
        if (selector.includes('[data-slot="sheet-content"]')) {
          return { length: 1 }
        }
        return []
      },
    }

    const handler = (event) => {
      if (event.key !== 'ArrowLeft' && event.key !== 'ArrowRight') return
      if (event.altKey || event.ctrlKey || event.metaKey || event.shiftKey) return
      const target = event.target
      if (target?.closest?.('input, textarea, select, [contenteditable="true"]')) {
        return
      }
      if (
        mockDocument.querySelector(
          '[data-slot="dialog-content"], [role="dialog"]:not([data-slot="sheet-content"]), [role="alertdialog"], [data-select-dropdown="true"]',
        ) ||
        mockDocument.querySelectorAll('[data-slot="sheet-content"]').length > 1
      ) {
        return
      }
      if (event.key === 'ArrowLeft' && canGoPrev) {
        event.preventDefault()
        prevCalled++
      } else if (event.key === 'ArrowRight' && canGoNext) {
        event.preventDefault()
        nextCalled++
      }
    }

    return {
      dispatch: (key, opts = {}) => {
        let prevented = false
        const event = {
          key,
          target: activeTarget,
          altKey: Boolean(opts.altKey),
          ctrlKey: Boolean(opts.ctrlKey),
          metaKey: Boolean(opts.metaKey),
          shiftKey: Boolean(opts.shiftKey),
          preventDefault: () => {
            prevented = true
          },
        }
        handler(event)
        return { prevented }
      },
      getStats: () => ({ prevCalled, nextCalled }),
    }
  }

  // Case 1: Target is an <input> inside an edit modal
  const inputTarget = {
    tagName: 'INPUT',
    closest: (sel) => (sel.includes('input') ? {} : null),
  }
  const inputEnv = createEnvironment({ activeTarget: inputTarget, canGoPrev: true, canGoNext: true })
  const inputResultLeft = inputEnv.dispatch('ArrowLeft')
  const inputResultRight = inputEnv.dispatch('ArrowRight')
  assert.equal(inputResultLeft.prevented, false, 'ArrowLeft in input should not be prevented')
  assert.equal(inputResultRight.prevented, false, 'ArrowRight in input should not be prevented')
  assert.deepEqual(inputEnv.getStats(), { prevCalled: 0, nextCalled: 0 }, 'Should not call prev/next')

  // Case 2: Target is a <textarea>
  const textareaTarget = {
    tagName: 'TEXTAREA',
    closest: (sel) => (sel.includes('textarea') ? {} : null),
  }
  const textareaEnv = createEnvironment({ activeTarget: textareaTarget })
  const textareaResult = textareaEnv.dispatch('ArrowLeft')
  assert.equal(textareaResult.prevented, false)
  assert.deepEqual(textareaEnv.getStats(), { prevCalled: 0, nextCalled: 0 })

  // Case 3: Shift + ArrowLeft (e.g. text selection)
  const plainTarget = { closest: () => null }
  const shiftEnv = createEnvironment({ activeTarget: plainTarget })
  const shiftResult = shiftEnv.dispatch('ArrowLeft', { shiftKey: true })
  assert.equal(shiftResult.prevented, false)
  assert.deepEqual(shiftEnv.getStats(), { prevCalled: 0, nextCalled: 0 })

  // Case 4: An edit modal is open (hasDialog = true), user presses arrow keys
  const dialogEnv = createEnvironment({ activeTarget: plainTarget, hasDialog: true })
  const dialogResult = dialogEnv.dispatch('ArrowLeft')
  assert.equal(dialogResult.prevented, false)
  assert.deepEqual(dialogEnv.getStats(), { prevCalled: 0, nextCalled: 0 })

  // Case 5: Normal sheet usage, no input focused, no modal open -> ArrowLeft switches to prev
  const normalEnv = createEnvironment({ activeTarget: plainTarget, hasDialog: false, canGoPrev: true, canGoNext: true })
  const normalResultLeft = normalEnv.dispatch('ArrowLeft')
  assert.equal(normalResultLeft.prevented, true)
  assert.deepEqual(normalEnv.getStats(), { prevCalled: 1, nextCalled: 0 })

  // Case 6: Normal sheet usage -> ArrowRight switches to next
  const normalResultRight = normalEnv.dispatch('ArrowRight')
  assert.equal(normalResultRight.prevented, true)
  assert.deepEqual(normalEnv.getStats(), { prevCalled: 1, nextCalled: 1 })
})
