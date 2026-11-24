import assert from 'node:assert/strict'
import test from 'node:test'
import { preferredOneOffModel, submitAccountOneOffTest } from './accountOneOffTest.ts'
import { PELICAN_PROMPT, QUALITY_TEST_DEFAULT_TIMEOUT_MINUTES } from './qualityTest.ts'

test('each shortcut posts once to the selected account and returns its persisted record ID', async t => {
  for (const [key, value] of Object.entries({ localStorage: { getItem: () => null }, window: new EventTarget() })) {
    const original = Object.getOwnPropertyDescriptor(globalThis, key)
    Object.defineProperty(globalThis, key, { configurable: true, value })
    t.after(() => original ? Object.defineProperty(globalThis, key, original) : delete globalThis[key])
  }
  const requests = []
  const responses = [Response.json({run:{id:18}}), Response.json({job:{id:22}}), Response.json({error:'Account busy'}, {status:409})]
  t.mock.method(globalThis, 'fetch', async (url, {method, body}) => {
    requests.push([method, url, JSON.parse(body)])
    return responses.shift()
  })
  assert.equal(preferredOneOffModel, 'gpt-6.2-sol')
  assert.equal(await submitAccountOneOffTest(3, 'intelligence', preferredOneOffModel, [], '糖果配对'), 18)
  assert.equal(await submitAccountOneOffTest(7, 'quality', 'other', ['high'], '鹈鹕骑自行车'), 22)
  await assert.rejects(submitAccountOneOffTest(7, 'quality', 'other', [''], 'Pelican'), /Account busy/)
  assert.deepEqual(requests, [
    ['POST', '/api/admin/accounts/3/intelligence-check/once', {model:'gpt-6.2-sol'}],
    ['POST', '/api/admin/accounts/7/quality-test', {model:'other', prompt:PELICAN_PROMPT, preset_key:'pelican', preset_name:'鹈鹕骑自行车', reasoning_effort:'high', timeout_minutes:QUALITY_TEST_DEFAULT_TIMEOUT_MINUTES}],
    ['POST', '/api/admin/accounts/7/quality-test', {model:'other', prompt:PELICAN_PROMPT, preset_key:'pelican', preset_name:'Pelican', reasoning_effort:'', timeout_minutes:QUALITY_TEST_DEFAULT_TIMEOUT_MINUTES}],
  ])
})
