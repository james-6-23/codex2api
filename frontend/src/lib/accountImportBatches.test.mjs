import assert from 'node:assert/strict';
import test from 'node:test';
import {
  exceedsFolderImportBatchLimit,
  MAX_FOLDER_IMPORT_BATCHES,
  runMixedAccountImport,
  sendAccountImportBatches,
  splitAccountImportFiles,
} from './accountImportBatches.ts';

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

const importPath = '/api/admin/accounts/import';
const folderPath = '/api/admin/accounts/folder/import';
const phaseHeader = 'X-Account-Import-Phase';
const batchHeader = 'X-Account-Import-Batch';
const sessionHeader = 'X-Account-Import-Session';

function makeForm(batch) {
  const form = new FormData();
  form.append('format', 'json_at');
  for (const file of batch) form.append('file', file.name);
  return form;
}

test('100 files use the existing endpoint; 101 or >150 MiB stage and prepare before committing', async () => {
  const files = Array.from({ length: 101 }, (_, i) => ({ size: 8192, name: `${i}.json` }));
  for (const [selection, expectedBatches] of [
    [files.slice(0, 100), [100]],
    [files, [100, 1]],
    [[{ size: 149 * MiB, name: 'large.json' }, { size: 2 * MiB, name: 'second.json' }], [1, 1]],
  ]) {
    const calls = [];
    const send = async (path, headers, body) => {
      const phase = headers[phaseHeader];
      calls.push({ path, phase, index: headers[batchHeader], session: headers[sessionHeader], body });
      return phase && phase !== 'commit'
        ? new Response(null, { status: 204, headers: { [sessionHeader]: 'test-folder' } })
        : Response.json({ total: 1 });
    };
    const progress = [];
    const batches = splitAccountImportFiles(selection);
    const responses = [];
    for await (const response of sendAccountImportBatches(batches, true, makeForm, send, value => progress.push(value))) {
      responses.push(response.status);
    }
    assert.deepEqual(responses, expectedBatches.map(() => 200));
    assert.deepEqual(calls.filter(call => call.body).map(call => call.body.getAll('file').length), expectedBatches);
    assert.deepEqual(calls.flatMap(call => call.body?.getAll('file') ?? []), selection.map(file => file.name));
    assert.equal(calls[0].body.get('format'), 'json_at');
    if (expectedBatches.length === 1) {
      assert.equal(calls[0].path, importPath);
      assert.equal(calls[0].phase, undefined);
      assert.deepEqual(progress, []);
    } else {
      assert.ok(calls.every(call => call.path === folderPath));
      assert.deepEqual(calls.map(call => call.phase), ['stage', 'stage-last', 'prepare', 'prepare', 'commit', 'commit']);
      assert.deepEqual(calls.map(call => call.index), ['0', '1', '0', '1', '0', '1']);
      assert.deepEqual(calls.map(call => call.session), [undefined, ...Array(5).fill('test-folder')]);
      assert.deepEqual(progress.filter(Boolean).map(p => p.current), [0, 1, 2, 3, 4]);
    }
  }
});

test('older servers cannot fall back to partially importing a JSON folder', async () => {
  const calls = [];
  const statuses = [];
  const send = async (path, headers) => {
    calls.push([path, headers[phaseHeader]]);
    return new Response('Not found', { status: 404 });
  };
  const batches = [[{ name: 'a.json' }], [{ name: 'b.json' }]];
  for await (const response of sendAccountImportBatches(batches, true, makeForm, send)) statuses.push(response.status);
  assert.deepEqual(calls, [[folderPath, 'stage']]);
  assert.deepEqual(statuses, [404]);
});

test('a later preparation failure cancels the folder without any commit', async () => {
  const phases = [];
  const send = async (_path, headers) => {
    const phase = headers[phaseHeader];
    phases.push(phase);
    if (phase === 'prepare' && headers[batchHeader] === '1') return Response.json({ error: 'fixture rejected' }, { status: 400 });
    return new Response(null, { status: 204, headers: { [sessionHeader]: 'test-folder' } });
  };
  const batches = [[{ name: 'a.json' }], [{ name: 'b.json' }]];
  for await (const response of sendAccountImportBatches(batches, true, makeForm, send)) {
    assert.equal(response.status, 400);
    break;
  }
  assert.deepEqual(phases, ['stage', 'stage-last', 'prepare', 'prepare', 'cancel']);
});

test('JSON selections exceeding 100 batches are rejected before sending any request', async () => {
  const batches = Array.from({ length: MAX_FOLDER_IMPORT_BATCHES + 1 }, (_, i) => [{ name: `${i}.json` }]);
  assert.equal(exceedsFolderImportBatchLimit(batches, true), true);
  assert.equal(exceedsFolderImportBatchLimit(batches.slice(0, MAX_FOLDER_IMPORT_BATCHES), true), false);
  assert.equal(exceedsFolderImportBatchLimit(batches, false), false);

  let called = 0;
  const send = async () => {
    called++;
    return new Response(null, { status: 204 });
  };
  await assert.rejects(async () => {
    for await (const _ of sendAccountImportBatches(batches, true, makeForm, send)) {
      // should not yield
    }
  }, /maximum batch count/);
  assert.equal(called, 0);
});

test('mixed JSON and TXT imports stop when JSON fails and continue when JSON succeeds', async () => {
  const order = [];
  const failed = await runMixedAccountImport(
    true,
    true,
    async () => {
      order.push('json');
      return false;
    },
    async () => {
      order.push('txt');
      return true;
    },
  );
  assert.equal(failed, false);
  assert.deepEqual(order, ['json']);

  order.length = 0;
  const succeeded = await runMixedAccountImport(
    true,
    true,
    async () => {
      order.push('json');
      return true;
    },
    async () => {
      order.push('txt');
      return true;
    },
  );
  assert.equal(succeeded, true);
  assert.deepEqual(order, ['json', 'txt']);
});
