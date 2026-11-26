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
