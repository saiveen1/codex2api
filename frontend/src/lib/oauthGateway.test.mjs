import test from 'node:test'
import assert from 'node:assert/strict'
import { parseMaterial, previewMatches, createSubmission, readPendingRequest, rememberPendingRequest, forgetPendingRequest, pendingRequestStorageKey } from './oauthGateway.ts'

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
