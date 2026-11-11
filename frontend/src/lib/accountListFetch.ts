// Skip the list request when a filter change still has a non-first page.
// The page reset renders once, and that render fetches the first page.
// Fetching the old page as well duplicates the accounts request.
export function shouldDeferAccountListFetch(
  page: number,
  filterKey: string,
  appliedFilterKey: string,
): boolean {
  return page !== 1 && filterKey !== appliedFilterKey
}
