export async function copyTextToClipboard(text: string) {
  if (window.isSecureContext && navigator.clipboard?.writeText) {
    try {
      await navigator.clipboard.writeText(text);
      return;
    } catch {
      // Fall back for browsers that block clipboard writes.
    }
  }

  const textarea = document.createElement("textarea");
  textarea.value = text;
  textarea.setAttribute("readonly", "true");
  textarea.style.position = "fixed";
  textarea.style.top = "0";
  textarea.style.left = "0";
  textarea.style.width = "1px";
  textarea.style.height = "1px";
  textarea.style.opacity = "0";
  textarea.style.pointerEvents = "none";
  // Radix moves focus back inside an open dialog when focus leaves its scope.
  const previous = document.activeElement;
  const host =
    previous?.closest('[role="dialog"], [role="alertdialog"]') ?? document.body;
  host.appendChild(textarea);
  try {
    textarea.focus({ preventScroll: true });
    textarea.select();
    textarea.setSelectionRange(0, text.length);
    if (!document.execCommand("copy")) throw new Error("copy failed");
  } finally {
    textarea.remove();
    if (previous instanceof HTMLElement && previous.isConnected)
      previous.focus({ preventScroll: true });
  }
}
