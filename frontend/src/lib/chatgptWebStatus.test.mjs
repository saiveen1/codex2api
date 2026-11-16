import test from 'node:test'
import assert from 'node:assert/strict'
import { webAccountStatus, webAccountStatusMessage } from './chatgptWebStatus.ts'

const state = (status, extra = {}) => ({ account_id: 1, enabled: true, status, capabilities: [], image_quota_remaining: null, ...extra })

test('unread Web state is not confused with an explicitly disabled account', () => {
  assert.equal(webAccountStatus(undefined), 'unknown')
  assert.equal(webAccountStatus(null), 'disabled')
  assert.equal(webAccountStatus(state('ready', { enabled: false })), 'disabled')
})

test('a ready record needs observation evidence and is not a live generation verdict', () => {
  assert.equal(webAccountStatus(state('ready')), 'unverified')
  assert.equal(webAccountStatus(state('ready', { observed_at: '2026-01-01T00:00:00Z' })), 'ready')
  assert.equal(webAccountStatusMessage(state('ready', { observed_at: '2026-01-01T00:00:00Z' })), 'chatgptWeb.statusReady')
})

test('actionable Web failures remain visible even without a successful observation', () => {
  for (const status of ['auth_required', 'challenge', 'quota_exhausted']) assert.equal(webAccountStatus(state(status)), status)
  assert.equal(webAccountStatus(state('unverified')), 'unverified')
  assert.equal(webAccountStatus(state('new-unrecognized-state')), 'unknown')
})
