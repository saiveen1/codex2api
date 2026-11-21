import assert from 'node:assert/strict'
import test from 'node:test'

import {
  DIAGNOSTIC_CONTEXT_MAX_BYTES,
  candidateCounts,
  candidateSamples,
  getDispatchDecision,
  usageDiagnosticStatus,
  diagnosticErrorNotes,
  getDiagnosticError,
  getDiagnosticRequestScopes,
  parseDiagnosticContext,
} from './diagnosticContext.ts'

test('diagnostic context safely distinguishes historical, invalid, and oversized values', () => {
  assert.deepEqual(parseDiagnosticContext(), { state: 'legacy' })
  assert.deepEqual(parseDiagnosticContext('not json'), { state: 'invalid', reason: 'invalid_json' })
  assert.deepEqual(parseDiagnosticContext('[]'), { state: 'invalid', reason: 'unexpected_shape' })

  const oversized = parseDiagnosticContext(`{"summary":"${'x'.repeat(DIAGNOSTIC_CONTEXT_MAX_BYTES)}"}`)
  assert.equal(oversized.state, 'oversize')
  if (oversized.state === 'oversize') {
    assert.ok(oversized.bytes > oversized.maxBytes)
  }
})

test('generic, omitted, and unavailable evidence retain their distinct meanings', () => {
  assert.match(
    diagnosticErrorNotes({ detail: 'generic' }).join('\n'),
    /具体根因当前不可得.*不能仅凭 HTTP 状态码判断/,
  )
  assert.match(
    diagnosticErrorNotes({ detail: 'generic', message_present: true, message_class: 'message_omitted' }).join('\n'),
    /上游曾提供错误消息.*已因隐私保护省略/,
  )
  assert.match(
    diagnosticErrorNotes({ detail: 'unavailable' }).join('\n'),
    /不表示上游一定没有提供任何内容/,
  )
  assert.match(
    diagnosticErrorNotes({ code_redacted: true }).join('\n'),
    /已被脱敏.*不是未提供/,
  )
})

test('diagnostic request scopes preserve ingress and outbound distinction', () => {
  const event = {
    request: { fingerprint: 'ingress-fingerprint', messages: 2 },
    attempt: {
      request_capture: 'captured',
      request: { fingerprint: 'outbound-fingerprint', messages: 1, bytes: 128 },
    },
  }
  const scopes = getDiagnosticRequestScopes(event)
  assert.equal(scopes.ingress?.fingerprint, 'ingress-fingerprint')
  assert.equal(scopes.outbound?.fingerprint, 'outbound-fingerprint')
  assert.notEqual(scopes.ingress, scopes.outbound)
  assert.equal(scopes.requestCapture, 'captured')
})

test('final event evidence wins over incomplete attempt evidence', () => {
  const event = {
    attempt: { error: { detail: 'unavailable' } },
    error: { detail: 'structured', source: 'downstream_response', message_present: true, message_class: 'message_omitted' },
  }
  assert.equal(getDiagnosticError(event), event.error)
  assert.match(diagnosticErrorNotes(getDiagnosticError(event)).join('\n'), /网关曾提供错误消息/)
  assert.equal(getDiagnosticError({ attempt: event.attempt }), event.attempt.error)
})

test('dispatch evidence separates Team authorization from authentication and never guesses missing fields', () => {
  const decision = {
    reason: 'no_available_account', scoped: 29, has_candidate: false, attempted: false,
    counts: { unauthorized: 28, account_key_denied: 1, cooldown: -1, concurrency_saturated: '10' },
    samples: [{ account_id: 11, reason: 'unauthorized' }, { account_id: 99, reason: 'account_key_denied' }, { account_id: -3, reason: 'available' }],
  }
  const result = getDispatchDecision({ decision })
  assert.equal(result.summary, '28 个账号鉴权不可用，1 个账号未授权给此 Key')
  assert.equal(result.dispatch, '未派发')
  assert.equal(getDispatchDecision({ decision: { ...decision, counts: { ...decision.counts, group_mismatch: 300 } } }).summary, result.summary)
  assert.equal(result.decision.has_candidate, false)
  assert.deepEqual(candidateCounts(decision.counts).map(({ code }) => code), ['unauthorized', 'account_key_denied'])
  assert.deepEqual(candidateSamples(decision.samples).map(({ id }) => id), [99, 11])
  assert.equal(getDispatchDecision({ decision: { reason: 'constructor' } }).label, '未能细分调度原因')
  assert.equal(getDispatchDecision({ decision: {} }), undefined)
})

test('status reasons distinguish gateway decisions, generic upstream failures, SSE HTTP 200 and history', () => {
  const row = (status_code, event) => ({ status_code, diagnostic_context: JSON.stringify(event) })
  assert.deepEqual(usageDiagnosticStatus(row(503)), { failed: true, label: '历史未采集', httpStatus: 503 })
  assert.equal(usageDiagnosticStatus(row(503, { stage: 'dispatch', decision: { reason: 'queue_full', attempted: false } })).label, '未派发 · 等待队列已满')
  assert.equal(usageDiagnosticStatus(row(429, { stage: 'dispatch', decision: { reason: 'quota_exhausted', attempted: false } })).label, '未派发 · 额度耗尽')
  assert.deepEqual(usageDiagnosticStatus(row(200, { stage: 'upstream_stream', downstream_http_status: 200, outcome_status: 503, error: { source: 'upstream_stream', detail: 'generic' } })), { failed: true, label: '上游未提供可定位原因', httpStatus: 200 })
  assert.deepEqual(usageDiagnosticStatus(row(200, { outcome: 'success' })), { failed: false })
})
