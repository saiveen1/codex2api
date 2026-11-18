import test from 'node:test'
import assert from 'node:assert/strict'
import { imageKeyMatchesSource, imageReferenceLimit, portalChannelReady, webImageModelOptions, webImageParams } from './imageStudioChannel.ts'

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
  assert.deepEqual(webImageParams({ model: 'other', size: '1024x1024', quality: 'high', background: 'transparent', output_format: 'bad', prompt: 'test' }), { model: 'other', size: 'auto', quality: 'auto', background: 'auto', output_format: 'png', prompt: 'test' })
  assert.equal(webImageParams({ size: '16:9' }).size, '16:9')
  assert.equal(imageReferenceLimit(true), 16)
  assert.equal(imageReferenceLimit(false), 4)
  assert.equal(imageReferenceLimit(false, 10), 10)
})

test('Web model choices preserve key permissions and selected aliases', () => {
  assert.deepEqual(webImageModelOptions(undefined), [])
  const models = webImageModelOptions({})
  assert.equal(models.length, 7)
  for (const { value } of models) assert.equal(webImageParams({ model: value }).model, value)
  assert.deepEqual(webImageModelOptions({ model_allow: ['GPT-IMAGE-2.5'], model_deny: ['gpt-image-2.5'] }).map(x => x.value), ['gpt-image-2.5'])
  assert.deepEqual(webImageModelOptions({ model_allow: ['other'] }), [])
  assert.equal(webImageModelOptions({ model_deny: ['gpt-image-2'] }).some(x => x.value === 'gpt-image-2'), false)
})

test('portal requires a channel snapshot for the current key', () => {
  assert.equal(portalChannelReady('key', undefined), false)
  assert.equal(portalChannelReady('key', 'old'), false)
  assert.equal(portalChannelReady('', ''), false)
  assert.equal(portalChannelReady('key', 'key'), true)
})
