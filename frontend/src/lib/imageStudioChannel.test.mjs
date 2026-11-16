import test from 'node:test'
import assert from 'node:assert/strict'
import { imageKeyMatchesSource, imageReferenceLimit, portalChannelReady, webImageParams } from './imageStudioChannel.ts'

test('Web requires an explicitly matching key; default retains automatic and other channels', () => {
  assert.equal(imageKeyMatchesSource('web', '', undefined, false), false)
  assert.equal(imageKeyMatchesSource('web', '1', 'codex', true), false)
  assert.equal(imageKeyMatchesSource('web', '1', 'chatgpt_web', true), true)
  assert.equal(imageKeyMatchesSource('default', '', undefined, false), true)
  assert.equal(imageKeyMatchesSource('default', '2', 'grok', true), true)
  assert.equal(imageKeyMatchesSource('default', '2', 'chatgpt_web', true), false)
  assert.equal(imageKeyMatchesSource('default', '2', undefined, false), false)
})

test('Web parameters normalize templates and reruns without changing unrelated fields', () => {
  assert.deepEqual(webImageParams({ model: 'other', size: '1024x1024', quality: 'high', background: 'transparent', output_format: 'bad', prompt: 'test' }), { model: 'gpt-image-2', size: 'auto', quality: 'auto', background: 'auto', output_format: 'png', prompt: 'test' })
  assert.equal(webImageParams({ size: '16:9' }).size, '16:9')
  assert.equal(imageReferenceLimit(true), 2)
  assert.equal(imageReferenceLimit(false), 4)
  assert.equal(imageReferenceLimit(false, 10), 10)
})

test('portal requires a channel snapshot for the current key', () => {
  assert.equal(portalChannelReady('key', undefined), false)
  assert.equal(portalChannelReady('key', 'old'), false)
  assert.equal(portalChannelReady('', ''), false)
  assert.equal(portalChannelReady('key', 'key'), true)
})
