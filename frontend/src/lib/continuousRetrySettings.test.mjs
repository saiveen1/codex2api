import assert from 'node:assert/strict'
import test from 'node:test'

import {
  buildContinuousRetryCatchAllPatch,
  buildContinuousRetryEnabledPatch,
  buildContinuousRetryHTTP400Patch,
  createContinuousRetrySaveQueue,
  parseContinuousRetryErrorCodes,
  parseContinuousRetryMaxDurationSeconds,
  parseContinuousRetryStatusCodes,
} from './continuousRetrySettings.ts'

test('continuous retry saves run in user action order even when requests overlap', async () => {
  const enqueue = createContinuousRetrySaveQueue()
  const events = []
  let releaseFirst
  const firstGate = new Promise((resolve) => { releaseFirst = resolve })

  const first = enqueue(async () => {
    events.push('first:start')
    await firstGate
    events.push('first:end')
    return 'first'
  })
  const second = enqueue(async () => {
    events.push('second:start')
    events.push('second:end')
    return 'second'
  })

  await Promise.resolve()
  assert.deepEqual(events, ['first:start'])
  releaseFirst()
  assert.deepEqual(await Promise.all([first, second]), ['first', 'second'])
  assert.deepEqual(events, ['first:start', 'first:end', 'second:start', 'second:end'])
})

test('continuous retry catch-all is one click and cannot remain active behind the master switch', () => {
  assert.deepEqual(buildContinuousRetryCatchAllPatch(true), {
    continuous_retry_enabled: true,
    continuous_retry_catch_all: true,
  })
  assert.deepEqual(buildContinuousRetryCatchAllPatch(false), {
    continuous_retry_catch_all: false,
  })
  assert.deepEqual(buildContinuousRetryEnabledPatch(false), {
    continuous_retry_enabled: false,
    continuous_retry_catch_all: false,
  })
})

test('continuous retry status-code drafts preserve multiple valid selections', () => {
  assert.deepEqual(
    parseContinuousRetryStatusCodes('503, 403,404,503,099,600,404x'),
    [403, 404, 503],
  )
})

test('continuous retry error-code drafts normalize exact machine tokens', () => {
  assert.deepEqual(
    parseContinuousRetryErrorCodes(' Rate_Limited,server.error,rate_limited,bad code!,context-error '),
    ['context-error', 'rate_limited', 'server.error'],
  )
})

test('continuous retry max duration is always bounded', () => {
  assert.equal(parseContinuousRetryMaxDurationSeconds(''), 600)
  assert.equal(parseContinuousRetryMaxDurationSeconds('-1'), 1)
  assert.equal(parseContinuousRetryMaxDurationSeconds('12.9'), 12)
  assert.equal(parseContinuousRetryMaxDurationSeconds('901'), 900)
})

test('HTTP 400 toggle preserves other status codes and leaves the source unchanged', () => {
  const original = [503, 403, 400, 503]
  assert.deepEqual(buildContinuousRetryHTTP400Patch(original, true), {
    continuous_retry_status_codes: [400, 403, 503],
  })
  assert.deepEqual(buildContinuousRetryHTTP400Patch(original, false), {
    continuous_retry_status_codes: [403, 503],
  })
  assert.deepEqual(original, [503, 403, 400, 503])
  assert.deepEqual(buildContinuousRetryHTTP400Patch([], true), { continuous_retry_status_codes: [400] })
  assert.deepEqual(buildContinuousRetryHTTP400Patch([400], false), { continuous_retry_status_codes: [] })
})

test('queued HTTP 400 toggle uses the latest saved status codes', async () => {
  const enqueue = createContinuousRetrySaveQueue()
  let settings = { continuous_retry_status_codes: [403] }
  const first = enqueue(async () => {
    settings = { continuous_retry_status_codes: [403, 503] }
  })
  const second = enqueue(async () => {
    settings = buildContinuousRetryHTTP400Patch(settings.continuous_retry_status_codes, true)
  })
  await Promise.all([first, second])
  assert.deepEqual(settings.continuous_retry_status_codes, [400, 403, 503])
})
