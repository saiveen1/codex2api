import assert from 'node:assert/strict'
import test from 'node:test'
import { readFileSync } from 'node:fs'
import { computeOutputTokensPerSec, formatTokensPerSec, hasMeasuredDuration } from './usageTiming.ts'

const base = { status_code: 200, output_tokens: 217, upstream_round_duration_ms: 8323 }

test('throughput uses the full upstream round and not downstream first or total timing', () => {
  assert.equal(computeOutputTokensPerSec(base).toFixed(2), '26.07')
  assert.equal(formatTokensPerSec(computeOutputTokensPerSec(base)), '26.07')
  for (const window of [19, 20, 21]) {
    for (const first of [0, 100, 8000]) {
      assert.equal(computeOutputTokensPerSec({ ...base, duration_ms: first + window, first_token_ms: first, upstream_first_content_ms: first }), computeOutputTokensPerSec(base))
    }
  }
})

test('hidden and final rounds both use their own output and full round duration', () => {
  const hidden = { ...base, hidden_continuation_round: true, upstream_round_index: 1 }
  const final = { ...base, output_tokens: 100, upstream_round_duration_ms: 4000, hidden_continuation_round: false, upstream_round_index: 2, duration_ms: 100000 }
  assert.equal(computeOutputTokensPerSec(hidden), 217 / 8323 * 1000)
  assert.equal(computeOutputTokensPerSec(final), 25)
  assert.equal(computeOutputTokensPerSec({ ...final, hidden_continuation_round: true }), 25)
})

test('completion fallback and missing, failed, empty or invalid values', () => {
  assert.equal(computeOutputTokensPerSec({ ...base, output_tokens: 0, completion_tokens: 217 }), computeOutputTokensPerSec(base))
  assert.equal(computeOutputTokensPerSec({ ...base, output_tokens: undefined, completion_tokens: 217 }), computeOutputTokensPerSec(base))
  assert.equal(computeOutputTokensPerSec({ status_code: 200, output_tokens: 217, duration_ms: 8323, first_token_ms: 100 }), null)
  for (const status of [0, 300, 304, 400, 500, NaN, Infinity]) assert.equal(computeOutputTokensPerSec({ ...base, status_code: status }), null)
  for (const value of [undefined, null, 0, -1, NaN, Infinity, -Infinity]) {
    assert.equal(hasMeasuredDuration(value), false) // SQL NULL is exposed as 0 in legacy rows.
    assert.equal(computeOutputTokensPerSec({ ...base, upstream_round_duration_ms: value }), null)
    assert.equal(computeOutputTokensPerSec({ ...base, output_tokens: value }), null)
  }
  assert.equal(computeOutputTokensPerSec({ ...base, output_tokens: NaN, completion_tokens: 217 }), null)
  assert.equal(computeOutputTokensPerSec({ ...base, output_tokens: Number.MAX_VALUE, upstream_round_duration_ms: Number.MIN_VALUE }), null)
})

test('both pages and mobile views use the shared cell and translations cover all languages', () => {
  const component = readFileSync(new URL('../components/UsageLogMetrics.tsx', import.meta.url), 'utf8')
  assert.match(component, /log\.hidden_continuation_round/)
  assert.match(component, /usage\.hiddenContinuationRound/)
  assert.match(component, /upstream_first_content_ms/)
  for (const page of ['Usage.tsx', 'APIKeyUsagePortal.tsx']) {
    const source = readFileSync(new URL(`../pages/${page}`, import.meta.url), 'utf8')
    assert.ok((source.match(/<TokensPerSecCell log=\{log\}/g) || []).length >= 2)
  }
  for (const locale of ['en', 'zh', 'zh-TW']) {
    const { usage } = JSON.parse(readFileSync(new URL(`../locales/${locale}.json`, import.meta.url), 'utf8'))
    for (const key of ['tableTokensPerSecHint', 'roundTimingNotRecorded', 'roundThroughputUnavailable', 'upstreamRoundDuration', 'upstreamFirstContent', 'hiddenContinuationRound', 'hiddenContinuation']) assert.ok(usage[key], `${locale}: ${key}`)
  }
})
