export interface CommandSearchItem {
  id: string
  label: string
  group: string
  description?: string
  keywords?: readonly string[]
}

/**
 * Case-insensitive token filter for the global Cmd+K command palette.
 * All whitespace-separated query tokens must match across label, description, group, or keywords.
 */
export function filterCommandItems<T extends CommandSearchItem>(items: readonly T[], rawQuery: string): T[] {
  const tokens = rawQuery
    .trim()
    .toLocaleLowerCase()
    .split(/\s+/)
    .filter(Boolean)

  if (tokens.length === 0) return [...items]

  return items.filter((item) => {
    const haystack = [
      item.label,
      item.description ?? '',
      item.group,
      ...(item.keywords ?? []),
    ]
      .join(' ')
      .toLocaleLowerCase()
    return tokens.every((token) => haystack.includes(token))
  })
}
