import test from 'node:test'
import assert from 'node:assert/strict'
import { parseMaterial, withAccountRuntimeProxies, previewMatches, createSubmission, readPendingRequest, rememberPendingRequest, forgetPendingRequest, pendingRequestStorageKey } from './oauthGateway.ts'

test('TXT preserves password whitespace and rejects ambiguous delimiters', () => {
  assert.deepEqual(parseMaterial('user---- pass ----totp\n', 'txt'), [{ identifier: 'user', password: ' pass ', totp_secret: 'totp' }])
  assert.throws(() => parseMaterial('user----p----extra----totp', 'txt'))
})
test('JSON preserves per-account proxy and enforces bounds', () => {
  const row = { identifier: 'a', password: ' b ', totp_secret: 't', proxy: 'socks5://proxy' }
  assert.deepEqual(parseMaterial(JSON.stringify([row]), 'json'), [row])
  assert.throws(() => parseMaterial(JSON.stringify(Array(101).fill(row)), 'json'))
  assert.throws(() => parseMaterial('x'.repeat(1048577), 'txt'))
  const structured = { ...row, proxy: { scheme: 'http', host: 'proxy.example.invalid', port: 8080, username: 'fixture', password: 'synthetic' } }
  assert.deepEqual(parseMaterial(JSON.stringify([structured]), 'json'), [structured])
  assert.throws(() => parseMaterial(JSON.stringify([{ ...row, expected_identity: {} }]), 'json'))
})
test('preview revision must match and submission keeps the supplied ID and content', () => {
  assert.equal(previewMatches({ revision: 4 }, 4), true)
  assert.equal(previewMatches({ revision: 4 }, 5), false)
  const input = { source_text: 'a---- b ----t', options: { proxy_mode: 'system' }, group_ids: [], save_credentials: false }
  const original = createSubmission(input, 'request-1')
  assert.deepEqual(original, { ...input, request_id: 'request-1' })
  assert.deepEqual(createSubmission(input, original.request_id), original)
})

test('only the opaque request ID survives navigation and invalid stored IDs are rejected', () => {
  const previous = globalThis.sessionStorage
  const values = new Map()
  globalThis.sessionStorage = { getItem: key => values.get(key) ?? null, setItem: (key, value) => values.set(key, value), removeItem: key => values.delete(key) }
  try {
    rememberPendingRequest('original-request-1')
    assert.equal(readPendingRequest(), 'original-request-1')
    assert.deepEqual([...values.entries()], [[pendingRequestStorageKey, 'original-request-1']])
    forgetPendingRequest()
    assert.equal(readPendingRequest(), null)
    values.set(pendingRequestStorageKey, '../unsafe')
    assert.equal(readPendingRequest(), null)
  } finally {
    if (previous === undefined) delete globalThis.sessionStorage
    else globalThis.sessionStorage = previous
  }
})

test('retention and an explicit recovery target survive preview submission unchanged', () => {
  const input = { source_text: 'a----p----t', options: { proxy_mode: 'system' }, group_ids: [], save_credentials: true, recovery_account_id: 42 }
  assert.deepEqual(createSubmission(input, 'repair-1'), { ...input, request_id: 'repair-1' })
  assert.equal(input.request_id, undefined)
})

test('unavailable storage prevents submission but cannot undo a confirmed acceptance', () => {
  const previous = globalThis.sessionStorage
  globalThis.sessionStorage = {
    getItem: () => { throw new Error('blocked') },
    setItem: () => { throw new Error('blocked') },
    removeItem: () => { throw new Error('blocked') },
  }
  try {
    assert.equal(readPendingRequest(), null)
    assert.throws(() => rememberPendingRequest('request-1'), /blocked/)
    assert.doesNotThrow(() => forgetPendingRequest())
  } finally {
    if (previous === undefined) delete globalThis.sessionStorage
    else globalThis.sessionStorage = previous
  }
})

test('runtime proxy overrides follow identity and never replace OAuth proxies', () => {
  const rows = parseMaterial(JSON.stringify([
    { identifier: 'One@example.invalid', password: ' pass ', totp_secret: 'totp', proxy: 'http://oauth.invalid', runtime_proxy: 'http://json-runtime.invalid' },
    { identifier: 'two@example.invalid', password: 'pass', totp_secret: 'totp' },
  ]), 'json')
  const overrides = { 'one@example.invalid': 'http://chosen-runtime.invalid', 'two@example.invalid': 'http://two-runtime.invalid' }
  const result = withAccountRuntimeProxies([...rows].reverse(), overrides)
  assert.equal(result[0].runtime_proxy, 'http://two-runtime.invalid')
  assert.equal(result[1].runtime_proxy, 'http://chosen-runtime.invalid')
  assert.equal(result[1].proxy, 'http://oauth.invalid')
  assert.equal(result[1].password, ' pass ')
  assert.equal(rows[0].runtime_proxy, 'http://json-runtime.invalid')
  assert.equal(withAccountRuntimeProxies(rows, { 'one@example.invalid': '' })[0].runtime_proxy, undefined)
  assert.equal(withAccountRuntimeProxies(rows, overrides, true)[0].runtime_proxy, undefined)
  assert.equal(withAccountRuntimeProxies(rows, overrides, true)[0].proxy, 'http://oauth.invalid')
  assert.throws(() => parseMaterial(JSON.stringify([{ ...rows[0], runtime_proxy: {} }]), 'json'))
})
