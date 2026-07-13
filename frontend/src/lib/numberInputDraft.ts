export interface DraftNumberOptions {
  integer?: boolean;
  min?: number;
  max?: number;
  emptyValue?: number;
}

export interface DraftNumberUpdate {
  draft: string;
  value: number;
  changed: boolean;
}

function normalizeDraftNumber(
  value: number,
  options: DraftNumberOptions,
): number {
  let normalized = value;
  if (typeof options.min === "number") {
    normalized = Math.max(options.min, normalized);
  }
  if (typeof options.max === "number") {
    normalized = Math.min(options.max, normalized);
  }
  return normalized;
}

export function parseDraftNumber(
  raw: string,
  options: DraftNumberOptions = {},
): number | null {
  const trimmed = raw.trim();
  if (!trimmed) return null;
  const parsed = Number(trimmed);
  if (!Number.isFinite(parsed)) return null;
  if (options.integer && !Number.isInteger(parsed)) return null;
  return normalizeDraftNumber(parsed, options);
}

export function updateDraftNumber(
  raw: string,
  currentValue: number,
  options: DraftNumberOptions = {},
): DraftNumberUpdate {
  const parsed = parseDraftNumber(raw, options);
  return {
    draft: raw,
    value: parsed ?? currentValue,
    changed: parsed !== null && parsed !== currentValue,
  };
}

export function commitDraftNumber(
  raw: string,
  currentValue: number,
  options: DraftNumberOptions = {},
): number {
  if (!raw.trim()) {
    return options.emptyValue ?? currentValue;
  }
  return parseDraftNumber(raw, options) ?? currentValue;
}
