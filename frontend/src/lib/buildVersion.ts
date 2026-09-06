// The date comes from the injected build ID, never from the browser clock.
export function buildVersionLabel(version: string): string {
  const match = /^local-(\d{4})(\d{2})(\d{2})-(\d{2})(\d{2})(?:-|$)/.exec(version)
  return match
    ? `本地版本 ${match[1]}-${match[2]}-${match[3]} ${match[4]}:${match[5]}`
    : version
}
