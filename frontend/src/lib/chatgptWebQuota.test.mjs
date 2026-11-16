import test from 'node:test'
import assert from 'node:assert/strict'
import { webImageQuotaRemaining } from './chatgptWebStatus.ts'

test('Web quota keeps zero and positive counts distinct from unknown', () => {
  assert.equal(webImageQuotaRemaining({ image_quota_remaining: 0 }), 0)
  assert.equal(webImageQuotaRemaining({ image_quota_remaining: 12 }), 12)
  assert.equal(webImageQuotaRemaining({ image_quota_remaining: null }), null)
  assert.equal(webImageQuotaRemaining(undefined), null)
  assert.equal(webImageQuotaRemaining(null), null)
})

test('invalid Web quota values never imply unlimited quota or a valid count', () => {
  for (const value of [-1, 0.5, NaN, Infinity, '0', Number.MAX_SAFE_INTEGER + 1]) {
    assert.equal(webImageQuotaRemaining({ image_quota_remaining: value }), null)
  }
})
