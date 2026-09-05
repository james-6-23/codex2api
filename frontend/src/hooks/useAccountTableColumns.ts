import { useCallback, useEffect, useMemo, useState } from "react";

export function useAccountTableColumns<Column extends string>(
  storageKey: string,
  columnOrder: readonly Column[],
) {
  const defaults = useMemo(
    () => Object.fromEntries(columnOrder.map((column) => [column, true])) as Record<Column, boolean>,
    [columnOrder],
  );
  const [columns, setColumns] = useState<Record<Column, boolean>>(() => {
    const initial = { ...defaults };
    try {
      const saved: unknown = JSON.parse(localStorage.getItem(storageKey) ?? "null");
      if (saved && typeof saved === "object") {
        for (const column of columnOrder) {
          const value = (saved as Record<string, unknown>)[column];
          if (typeof value === "boolean") initial[column] = value;
        }
      }
    } catch {
      // Use defaults if browser storage is unavailable or contains invalid data.
    }
    return initial;
  });

  useEffect(() => {
    try {
      localStorage.setItem(storageKey, JSON.stringify(columns));
    } catch {
      // Column selection still works when browser storage is unavailable.
    }
  }, [columns, storageKey]);

  const toggleColumn = useCallback((column: Column) => {
    setColumns((current) => ({ ...current, [column]: !current[column] }));
  }, []);
  const resetColumns = useCallback(() => setColumns({ ...defaults }), [defaults]);

  return { columns, toggleColumn, resetColumns };
}
