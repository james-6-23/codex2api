const MAX_BATCH_BYTES = 150 * 1024 * 1024;
const MAX_BATCH_FILES = 100; // Leave room for non-file multipart fields, even with tiny files.

export function splitAccountImportFiles<T extends { size: number }>(files: readonly T[]): T[][] {
  const batches: T[][] = [];
  let batch: T[] = [];
  let bytes = 0;
  for (const file of files) {
    if (batch.length && (batch.length >= MAX_BATCH_FILES || bytes + file.size > MAX_BATCH_BYTES)) {
      batches.push(batch);
      batch = [];
      bytes = 0;
    }
    batch.push(file);
    bytes += file.size;
  }
  if (batch.length) batches.push(batch);
  return batches;
}

const importPath = "/api/admin/accounts/import";
const folderImportPath = "/api/admin/accounts/folder/import";
const phaseHeader = "X-Account-Import-Phase";
const batchHeader = "X-Account-Import-Batch";
const sessionHeader = "X-Account-Import-Session";

export async function* sendAccountImportBatches<T>(
  batches: readonly T[][],
  isJSON: boolean,
  makeForm: (batch: T[]) => FormData,
  send: (path: string, headers: Record<string, string>, body?: FormData) => Promise<Response>,
  onPreflight?: (progress: { current: number; total: number } | null) => void,
): AsyncGenerator<Response> {
  if (!isJSON || batches.length < 2) {
    for (const batch of batches) yield await send(importPath, {}, makeForm(batch));
    return;
  }

  let session = "";
  const request = (phase: string, index: number, body?: FormData) =>
    send(folderImportPath, {
      [phaseHeader]: phase,
      [batchHeader]: String(index),
      ...(session ? { [sessionHeader]: session } : {}),
    }, body);
  const total = batches.length * 2;
  try {
    onPreflight?.({ current: 0, total });
    for (let i = 0; i < batches.length; i++) {
      const response = await request(i === batches.length - 1 ? "stage-last" : "stage", i, makeForm(batches[i]));
      if (!response.ok) {
        yield response;
        return;
      }
      session = session || response.headers.get(sessionHeader) || "";
      if (!session || response.status !== 204) throw new Error("Account import staging was not acknowledged");
      onPreflight?.({ current: i + 1, total });
    }
    for (let i = 0; i < batches.length; i++) {
      const response = await request("prepare", i);
      if (!response.ok) {
        yield response;
        return;
      }
      if (response.status !== 204) throw new Error("Account import preparation was not acknowledged");
      onPreflight?.({ current: batches.length + i + 1, total });
    }
    onPreflight?.(null);
    // All upload and identity checks must finish before requesting any account writes.
    for (let i = 0; i < batches.length; i++) yield await request("commit", i);
    session = "";
  } finally {
    onPreflight?.(null);
    // A disconnected tab cannot clean up; the server also expires abandoned sessions.
    if (session) void request("cancel", 0).catch(() => {});
  }
}
