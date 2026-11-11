// One delayed stats reload. A hidden document does not drop the poll: the
// callback waits until the document is visible, and cleanup removes that
// listener along with the timer.

type AccountStatsPollDeps = {
  setTimeout: (callback: () => void, delayMs: number) => number
  clearTimeout: (timer: number) => void
  visibility: {
    readonly hidden: boolean
    addEventListener: (type: "visibilitychange", listener: () => void) => void
    removeEventListener: (type: "visibilitychange", listener: () => void) => void
  }
}

const browserAccountStatsPollDeps = (): AccountStatsPollDeps => ({
  setTimeout: (callback, delayMs) => window.setTimeout(callback, delayMs),
  clearTimeout: (timer) => window.clearTimeout(timer),
  visibility: {
    get hidden() {
      return document.hidden
    },
    addEventListener: (type, listener) => {
      document.addEventListener(type, listener)
    },
    removeEventListener: (type, listener) => {
      document.removeEventListener(type, listener)
    },
  },
})

export function scheduleAccountStatsPoll(
  delayMs: number,
  onFire: () => void,
  deps: AccountStatsPollDeps = browserAccountStatsPollDeps(),
): () => void {
  let visibilityHandler: (() => void) | null = null
  const timer = deps.setTimeout(() => {
    if (!deps.visibility.hidden) {
      onFire()
      return
    }
    visibilityHandler = () => {
      if (deps.visibility.hidden || visibilityHandler === null) return
      deps.visibility.removeEventListener("visibilitychange", visibilityHandler)
      visibilityHandler = null
      onFire()
    }
    deps.visibility.addEventListener("visibilitychange", visibilityHandler)
  }, delayMs)

  return () => {
    deps.clearTimeout(timer)
    if (visibilityHandler === null) return
    deps.visibility.removeEventListener("visibilitychange", visibilityHandler)
    visibilityHandler = null
  }
}
