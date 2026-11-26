import assert from 'node:assert/strict';
import test from 'node:test';
import { splitAccountImportFiles } from './accountImportBatches.ts';

const MiB = 1024 * 1024;

test('small account files stay ordered, with room for extra multipart fields', () => {
  const files = Array.from({ length: 1000 }, (_, i) => ({ size: 6200, name: `${i}.json` }));
  const batches = splitAccountImportFiles(files);
  assert.deepEqual(batches.map(batch => batch.length), Array(10).fill(100));
  assert.deepEqual(batches.flat(), files);
});

test('byte limit is inclusive; a larger allowed single file stays alone', () => {
  const files = [149, 1, 2, 151, 1].map(size => ({ size: size * MiB }));
  assert.deepEqual(splitAccountImportFiles(files), [files.slice(0, 2), [files[2]], [files[3]], [files[4]]]);
  assert.deepEqual(splitAccountImportFiles([]), []);
});
