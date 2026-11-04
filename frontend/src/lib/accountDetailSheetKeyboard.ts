/**
 * Account detail sheet keyboard navigation guards.
 *
 * Provides predicates for filtering ArrowLeft and ArrowRight navigation
 * to avoid hijacking text input cursor movement and conflicting with active modals.
 */

export interface DocumentOverlayQueryTarget {
  querySelector(selectors: string): unknown;
  querySelectorAll(selectors: string): { length: number };
}

export interface AccountDetailKeyboardEventLike {
  key: string;
  altKey?: boolean;
  ctrlKey?: boolean;
  metaKey?: boolean;
  shiftKey?: boolean;
  target?: EventTarget | null;
}

/**
 * Determines whether an event target is an interactive or editable element
 * (input, textarea, select, or contenteditable) where arrow keys should control native cursor movement.
 *
 * @param target - The event target to inspect.
 * @returns True if the target or its ancestor is an editable element.
 */
export function isEditableKeyboardTarget(target: EventTarget | null): boolean {
  if (!target || typeof (target as HTMLElement).closest !== "function") {
    return false;
  }
  return Boolean(
    (target as HTMLElement).closest(
      'input, textarea, select, [contenteditable="true"]',
    ),
  );
}

/**
 * Checks whether an active dialog, alert dialog, select dropdown,
 * or stacked secondary sheet is present in the document.
 *
 * @param doc - Document or mock document containing querySelector and querySelectorAll.
 * @returns True if a blocking modal or overlay is active.
 */
export function hasBlockingModalOrOverlay(
  doc: DocumentOverlayQueryTarget = document,
): boolean {
  if (typeof doc?.querySelector !== "function") {
    return false;
  }
  if (
    doc.querySelector(
      '[data-slot="dialog-content"], [role="dialog"]:not([data-slot="sheet-content"]), [role="alertdialog"], [data-select-dropdown="true"]',
    )
  ) {
    return true;
  }
  if (typeof doc?.querySelectorAll === "function") {
    return doc.querySelectorAll('[data-slot="sheet-content"]').length > 1;
  }
  return false;
}

/**
 * Evaluates whether an incoming keyboard event should navigate account detail sheets.
 * Filters out modified keys, editable inputs, and events occurring while dialogs or overlays are active.
 *
 * @param event - The keyboard event or compatible object to evaluate.
 * @param doc - Optional document-like target for overlay queries.
 * @returns True if arrow navigation should proceed.
 */
export function shouldHandleAccountDetailKeyNavigation(
  event: AccountDetailKeyboardEventLike,
  doc: DocumentOverlayQueryTarget = document,
): boolean {
  if (event.key !== "ArrowLeft" && event.key !== "ArrowRight") {
    return false;
  }
  if (event.altKey || event.ctrlKey || event.metaKey || event.shiftKey) {
    return false;
  }
  if (isEditableKeyboardTarget(event.target ?? null)) {
    return false;
  }
  if (hasBlockingModalOrOverlay(doc)) {
    return false;
  }
  return true;
}
