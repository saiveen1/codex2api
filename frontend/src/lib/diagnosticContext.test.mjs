import assert from 'node:assert/strict'
import test from 'node:test'

import {
  DIAGNOSTIC_CONTEXT_MAX_BYTES,
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
