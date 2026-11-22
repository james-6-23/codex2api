export function selectAutoScrollKey(
  open: boolean,
  positionReady: boolean,
  value: string,
): string | null {
  if (!open || !positionReady) return null;
  return value;
}

// Radix Dialog 的 react-remove-scroll 会在 document capture 阶段对弹层外
// （含 createPortal 到 body 的下拉）的 wheel/touchmove 调 preventDefault，
// 导致 overflow 容器无法原生滚动。这里手动改 scrollTop/scrollLeft 绕过。
// 返回 true 表示确实滚动了（调用方此时应 preventDefault 阻止原生滚动，避免双倍滚动）。
export function applyManualScroll(
  el: HTMLElement,
  deltaX: number,
  deltaY: number,
): boolean {
  const canScrollY = el.scrollHeight > el.clientHeight + 1;
  const canScrollX = el.scrollWidth > el.clientWidth + 1;
  if (!canScrollY && !canScrollX) return false;

  let scrolled = false;
  if (canScrollY && deltaY !== 0) {
    const maxTop = el.scrollHeight - el.clientHeight;
    const next = Math.min(maxTop, Math.max(0, el.scrollTop + deltaY));
    if (next !== el.scrollTop) {
      el.scrollTop = next;
      scrolled = true;
    }
  }
  if (canScrollX && deltaX !== 0) {
    const maxLeft = el.scrollWidth - el.clientWidth;
    const next = Math.min(maxLeft, Math.max(0, el.scrollLeft + deltaX));
    if (next !== el.scrollLeft) {
      el.scrollLeft = next;
      scrolled = true;
    }
  }
  return scrolled;
}
