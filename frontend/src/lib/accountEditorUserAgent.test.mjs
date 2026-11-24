import assert from 'node:assert/strict'
import { readFileSync } from 'node:fs'
import { runInNewContext } from 'node:vm'
import test from 'node:test'

const source = readFileSync(new URL('../pages/Accounts.tsx', import.meta.url), 'utf8')
function block(start, end) {
  const from = source.indexOf(start), to = source.indexOf(end, from + start.length)
  assert.ok(from >= 0 && to > from)
  return source.slice(from, to)
}
const derived = block('const manualCodexUA =', 'const [editCodexFingerprintMode')
const save = block('const handleSaveScheduler =', 'const handleSaveAccountEditor =')
const close = block('const closeSchedulerEditor =', 'const parsedScoreBias =')
const noop = () => {}

// Run the dialog's save handler against a recording API. No server or real accounts.
async function submit(original, draft, relay = false) {
  const writes = [], errors = []
  const context = {
    editingAccount: { id: 42, codex_user_agent: original }, editCodexUA: draft,
    api: { updateAccountScheduler: (id, payload) => writes.push({ id, payload }) },
    showToast: (message, kind) => { if (kind === 'error') errors.push(message) }, t: key => key,
    getErrorMessage: String, isCodexOfficialAccount: () => !relay, setEditSubmitting: noop,
    parseCustomHeadersText: () => ({ ok: true, value: { 'X-Example': 'kept' } }), editCustomHeadersText: '{}',
    scoreMode: 'default', concurrencyMode: 'default', editProxyUrl: '', editTimezone: '',
    accountGroupChanges: () => ({}), editGroupIds: [], allowedAPIKeySelection: [], editTags: [],
    percentThresholdInputToRatio: noop, dispatchCountLimitInputToValue: noop, schedulerPriorityInputToValue: noop,
    reload: noop, reloadGroups: noop, closeSchedulerEditor: noop,
  }
  for (const key of ['scoreInputInvalid','concurrencyInputInvalid','editAutoPause5hThresholdInvalid','editAutoPause7dThresholdInvalid','editDispatchCountLimitInvalid','editSchedulerPriorityInvalid','skipWarmTier','keepConcurrencyOnDegrade','editAutoPause5hThresholdInput','editAutoPause7dThresholdInput','editAutoPause5hDisabled','editAutoPause7dDisabled','editIgnoreUsageLimitStatusMode','editDispatchCountLimitInput','editSchedulerPriorityInput','editForceFast','editCodexFingerprintMode']) context[key] = false
  await runInNewContext(`${derived}\n${save}\nhandleSaveScheduler()`, context)
  return { writes, errors }
}

test('account dialog saves preset, manual and inherit only against the edited account', async () => {
  for (const [old, draft, want] of [['', 'codex-vscode', 'codex-vscode'], ['codex-vscode', 'raw: Example/1.2 ', 'raw:Example/1.2'], ['raw:Example/1.2', '', '']]) {
    const { writes, errors } = await submit(old, draft)
    assert.deepEqual(errors, [])
    assert.equal(writes.length, 1); assert.equal(writes[0].id, 42)
    assert.equal(writes[0].payload.codex_user_agent, want)
    assert.equal(writes[0].payload.custom_headers['X-Example'], 'kept')
  }
  for (const args of [['codex-exec', 'codex-exec'], ['', 'codex-exec', true]]) {
    assert.equal('codex_user_agent' in (await submit(...args)).writes[0].payload, false)
  }
  for (const ua of ['raw: ', 'raw:bad\nvalue']) assert.equal((await submit('', ua)).writes.length, 0)
})

test('screenshot dialog hosts both UA controls, opens from the account, and cancel discards the draft', () => {
  const section = block('{/* 绑定时区 */}', '{/* 自定义请求头 */}')
  assert.match(section, /id="account-codex-ua"/)
  assert.match(section, /setEditCodexUA\(bindCodexPreset\(kind,/)
  assert.match(section, /<CodexPresetSystemSelect binding=\{editCodexUA\} onChange=\{setEditCodexUA\}/)
  assert.match(section, /setEditCodexUA\(`raw:\$\{event.target.value\}`\)/)
  assert.doesNotMatch(section, /api\./)
  assert.match(block('const populateSchedulerEditor =', 'const openSchedulerEditor ='), /setEditCodexUA\(account.codex_user_agent \?\? ""\)/)
  const context = { editSubmitting: false, emptyModelMappingEntries: () => [] }
  for (const [, setter] of close.matchAll(/\b(set[A-Z]\w+)\(/g)) context[setter] = noop
  let draft = 'raw:unsaved'
  context.setEditCodexUA = value => { draft = value }
  runInNewContext(`${close}\ncloseSchedulerEditor()`, context) // No API in this context.
  assert.equal(draft, '')
})
