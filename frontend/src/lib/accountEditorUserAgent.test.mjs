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
async function submit(original, draft, relay = false, system = '', pending = false, versionError = false) {
  const writes = [], errors = []
  const context = {
    editingAccount: { id: 42, codex_user_agent: original }, editCodexUA: draft,
    editCodexSystem: system, editCodexVersionPending: pending, editCodexVersionError: versionError,
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
  for (const args of [['', '', false, 'windows'], ['', 'codex-vscode@windows', false, 'windows', true], ['', 'codex-vscode@windows', false, 'windows', false, true]]) assert.equal((await submit(...args)).writes.length, 0)
  assert.equal((await submit('', 'codex-vscode@windows', false, 'windows')).writes[0].payload.codex_user_agent, 'codex-vscode@windows')
  assert.equal((await submit('', 'raw:client/1.2 (app; 3.4)', false, 'windows')).writes[0].payload.codex_user_agent, 'raw:client/1.2 (app; 3.4)')
})

test('version choice previews the selected platform and pins only an exact matching response', async () => {
  const body = block('const value = event.target.value;\n                                  const request'.replaceAll('\n', source.includes('\r\n') ? '\r\n' : '\n'), '}}>').replace('api.previewCodexUserAgent(', 'return api.previewCodexUserAgent(')
  for (const matches of [true, false]) {
    let sent, pinned, failed = false, pending
    const platform = { os_name: 'Mac OS', os_version: '15.0', arch: 'arm64' }
    const context = { event: { target: { value: '1' } }, kind: 'codex-tui', platform, editCodexRequest: { current: 0 },
      pairs: [{ cli_version: '1.2.3', app_version: '1.2.3' }], setEditCodexVersion: noop, setEditCodexVersionKind: noop,
      setEditCodexVersionError: v => { failed = v }, setEditCodexVersionPending: v => { pending = v }, setEditCodexUA: v => { pinned = v },
      api: { previewCodexUserAgent: async ({ config }) => { sent = JSON.parse(config); return { persona: { version: matches ? '1.2.3' : '1.2.4', app_version: '1.2.3', user_agent: 'synthetic/1.2.3' } } } },
    }
    await runInNewContext(`(async () => { ${body} })()`, context)
    assert.equal(sent.os_name, platform.os_name); assert.equal(sent.client_version, '1.2.3'); assert.equal(sent.client_kind, 'codex-tui')
    assert.equal(pinned, matches ? 'raw:synthetic/1.2.3' : undefined); assert.equal(failed, !matches); assert.equal(pending, false)
  }
})

test('screenshot dialog hosts both UA controls, opens from the account, and cancel discards the draft', () => {
  const section = block('{/* 绑定时区 */}', '{/* 自定义请求头 */}')
  assert.match(section, /id="account-codex-ua"/)
  assert.ok(section.indexOf('value={editCodexSystem}') < section.indexOf('id="account-codex-ua" className'))
  assert.match(section, /presetSystemOptions\(option.value\)/)
  assert.match(section, /spec\?\.version_pairs/)
  assert.match(section, /api\.previewCodexUserAgent/)
  assert.match(section, /preview\.persona\?\.version !== pair\.cli_version/)
  assert.match(section, /preview\.persona\?\.app_version !== pair\.app_version/)
  assert.match(section, /setEditCodexUA\(`raw:\$\{ua\}`\)/)
  assert.match(section, /setEditCodexUA\(`raw:\$\{event.target.value\}`\)/)
  assert.doesNotMatch(section, /api\.update/)
  assert.match(block('const populateSchedulerEditor =', 'const openSchedulerEditor ='), /setEditCodexUA\(account.codex_user_agent \?\? ""\)/)
  const context = { editSubmitting: false, emptyModelMappingEntries: () => [], editCodexRequest: { current: 0 } }
  for (const [, setter] of close.matchAll(/\b(set[A-Z]\w+)\(/g)) context[setter] = noop
  let draft = 'raw:unsaved'
  context.setEditCodexUA = value => { draft = value }
  runInNewContext(`${close}\ncloseSchedulerEditor()`, context) // No API in this context.
  assert.equal(draft, '')
})
