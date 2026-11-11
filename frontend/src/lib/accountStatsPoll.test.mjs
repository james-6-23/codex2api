import assert from "node:assert/strict"
import test from "node:test"
import { scheduleAccountStatsPoll } from "./accountStatsPoll.ts"

const POLL_DELAY_MS = 3000

function createClock() {
  let now = 0
  let nextID = 1
  const timers = new Map()
  return {
    setTimeout(callback, delayMs) {
      const id = nextID++
      timers.set(id, { callback, at: now + delayMs })
      return id
    },
    clearTimeout(id) {
      timers.delete(id)
    },
    advance(ms) {
      now += ms
      const due = [...timers.entries()]
        .filter(([, timer]) => timer.at <= now)
        .sort((left, right) => left[1].at - right[1].at || left[0] - right[0])
      for (const [id, timer] of due) {
        if (!timers.has(id)) continue
        timers.delete(id)
        timer.callback()
      }
    },
  }
}

function createVisibility(hidden) {
  const listeners = new Set()
  return {
    hidden,
    listenerCount() {
      return listeners.size
    },
    addEventListener(type, listener) {
      assert.equal(type, "visibilitychange")
      listeners.add(listener)
    },
    removeEventListener(type, listener) {
      assert.equal(type, "visibilitychange")
      listeners.delete(listener)
    },
    setHidden(next) {
      this.hidden = next
      for (const listener of [...listeners]) listener()
    },
  }
}

function arm(hidden) {
  const clock = createClock()
  const visibility = createVisibility(hidden)
  const fires = []
  const cancel = scheduleAccountStatsPoll(
    POLL_DELAY_MS,
    () => {
      fires.push(visibility.hidden)
    },
    { ...clock, visibility },
  )
  return { clock, visibility, fires, cancel }
}

test("visible document fires one stats poll after the existing delay", () => {
  const poll = arm(false)
  poll.clock.advance(POLL_DELAY_MS - 1)
  assert.deepEqual(poll.fires, [])
  poll.clock.advance(1)
  assert.deepEqual(poll.fires, [false])
  assert.equal(poll.visibility.listenerCount(), 0)
  poll.clock.advance(POLL_DELAY_MS)
  assert.deepEqual(poll.fires, [false])
})

test("hidden document resumes the stats poll when it becomes visible", () => {
  const poll = arm(true)
  poll.clock.advance(POLL_DELAY_MS)
  assert.deepEqual(poll.fires, [])
  assert.equal(poll.visibility.listenerCount(), 1)

  poll.visibility.setHidden(true)
  assert.deepEqual(poll.fires, [])
  assert.equal(poll.visibility.listenerCount(), 1)

  poll.visibility.setHidden(false)
  assert.deepEqual(poll.fires, [false])
  assert.equal(poll.visibility.listenerCount(), 0)

  poll.visibility.setHidden(true)
  poll.visibility.setHidden(false)
  assert.deepEqual(poll.fires, [false])
})

test("cleanup removes the deferred visibility listener and the pending timer", () => {
  const waiting = arm(true)
  waiting.clock.advance(POLL_DELAY_MS)
  waiting.cancel()
  assert.equal(waiting.visibility.listenerCount(), 0)
  waiting.visibility.setHidden(false)
  assert.deepEqual(waiting.fires, [])

  const pending = arm(false)
  pending.cancel()
  pending.clock.advance(POLL_DELAY_MS)
  assert.deepEqual(pending.fires, [])
  assert.equal(pending.visibility.listenerCount(), 0)
})
