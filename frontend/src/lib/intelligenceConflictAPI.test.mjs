import test from 'node:test'
import assert from 'node:assert/strict'
import { api, AdminAPIError } from '../api.ts'
import { intelligenceTargetConflict } from './intelligenceCheck.ts'

test('settings API preserves structured conflicts while retaining legacy error messages', async t => {
  const originalStorage = Object.getOwnPropertyDescriptor(globalThis, 'localStorage')
  Object.defineProperty(globalThis, 'localStorage', {configurable:true, value:{getItem:()=> 'fixture'}})
  t.after(() => {
    if (originalStorage) Object.defineProperty(globalThis, 'localStorage', originalStorage)
    else delete globalThis.localStorage
  })
  const details = {error:'incompatible target', code:'intelligence_check_target_conflict',target_group_id:8,target_group_channel:'codex',conflict_count:1,conflicts:[{account_id:68876,account_name:'Claude',channel:'claude'}]}
  let body = JSON.stringify(details)
  t.mock.method(globalThis, 'fetch', async () => new Response(body, {status:400}))
  await assert.rejects(api.saveIntelligenceSettings({revision:1}), error => {
    assert.ok(error instanceof AdminAPIError)
    assert.equal(error.status,400)
    assert.equal(error.message,'incompatible target')
    assert.deepEqual(intelligenceTargetConflict(error), details)
    return true
  })
  body = 'upstream unavailable'
  await assert.rejects(api.saveIntelligenceSettings({revision:1}), error => {
    assert.equal(error.message,body)
    assert.equal(intelligenceTargetConflict(error),null)
    return true
  })
})

test('intelligence history uses summary and batch trial explicitly sends record_only', async t => {
  const originalStorage = Object.getOwnPropertyDescriptor(globalThis, 'localStorage')
  Object.defineProperty(globalThis, 'localStorage', {configurable:true, value:{getItem:()=> 'fixture'}})
  t.after(() => {
    if (originalStorage) Object.defineProperty(globalThis, 'localStorage', originalStorage)
    else delete globalThis.localStorage
  })
  const requests = []
  t.mock.method(globalThis, 'fetch', async (url, init) => {
    requests.push({url:new URL(url, 'https://test.invalid'), init})
    return Response.json(init.method === 'POST' ? {runs:[], skipped_account_ids:[9]} : {items:[]})
  })
  await api.getIntelligenceRunSummaries(2, {group_id:42, plan:'plus', source:'manual', record_only:'false', before:'2026-10-15T00:00:00Z'})
  await api.getIntelligenceAccounts(2, {group_id:42, plan:'plus', page_size:100})
  assert.deepEqual(await api.createIntelligenceRuns([9], true), {runs:[], skipped_account_ids:[9]})
  await api.createIntelligenceRuns([10])
  assert.match(requests[0].url.pathname, /\/intelligence-check\/runs$/)
  assert.deepEqual(Object.fromEntries(requests[0].url.searchParams), {page:'2', page_size:'20', group_id:'42', plan:'plus', source:'manual', record_only:'false', before:'2026-10-15T00:00:00Z', summary:'1'})
  assert.match(requests[1].url.pathname, /\/intelligence-check\/accounts$/)
  assert.deepEqual(Object.fromEntries(requests[1].url.searchParams), {page:'2', page_size:'100', group_id:'42', plan:'plus'})
  assert.deepEqual(JSON.parse(requests[2].init.body), {account_ids:[9], record_only:true})
  assert.deepEqual(JSON.parse(requests[3].init.body), {account_ids:[10]})
})
