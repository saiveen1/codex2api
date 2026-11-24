import assert from 'node:assert/strict'
import test from 'node:test'
import { KEY_USAGE_VIEWS, normalizeKeyUsageView, visibleKeyUsageViews } from './keyUsageViews.ts'

test('authorized keys retain the accounts tab and direct route', () => {
  const views = visibleKeyUsageViews(true)
  assert.deepEqual(views, ['overview', 'quota', 'accounts', 'logs'])
  for (const view of KEY_USAGE_VIEWS) assert.equal(normalizeKeyUsageView(view, views), view)
})

test('unavailable accounts routes fall back without removing other tabs', () => {
  const views = visibleKeyUsageViews(false)
  assert.deepEqual(views, ['overview', 'quota', 'logs'])
  for (const view of ['accounts', 'unknown', undefined]) assert.equal(normalizeKeyUsageView(view, views), 'overview')
  for (const view of views) assert.equal(normalizeKeyUsageView(view, views), view)
})
