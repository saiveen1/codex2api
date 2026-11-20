import test from 'node:test'
import assert from 'node:assert/strict'
import { intelligenceWindow, intelligenceColor, intelligenceQuery, latestRequestGuard, intelligenceTargetConflict, intelligenceScheduleLabel, intelligenceGroupNames, intelligencePolicyName, intelligenceRuleError, intelligenceTimeRange, normalizeIntelligenceRule } from './intelligenceCheck.ts'
test('last 20 completed checks remain chronological and errors do not dilute pass rate', () => {
  const records = Array.from({ length: 23 }, (_, i) => ({ id: i, status: i % 3 === 0 ? 'normal' : i % 3 === 1 ? 'degraded' : 'error', created_at: new Date(i * 1000).toISOString() }))
  records.push({ id: 24, status: 'running' })
  const result = intelligenceWindow(records.toReversed())
  assert.equal(result.records.length, 20)
  assert.equal(result.records[0].id, 3)
  assert.equal(result.records.at(-1).id, 22)
  assert.equal(result.rate, 50)
  assert.equal(result.errors, 6)
})
test('no scored samples never display 100 percent, and specified colors stay distinct', () => {
  assert.equal(intelligenceWindow([]).rate, null)
  assert.equal(intelligenceWindow([{ id: 1, status: 'error' }]).rate, null)
  assert.equal(intelligenceColor('normal'), '#22c55e')
  assert.equal(intelligenceColor('degraded'), '#ef4444')
  assert.equal(intelligenceColor('error'), '#991b1b')
  assert.equal(intelligenceColor('interrupted'), undefined)
})
test('queries escape filters and stale async responses are rejected after newer load or disposal', () => {
  assert.equal(intelligenceQuery(2, { search: 'a&b', channel: '', account_id: 42 }), 'page=2&page_size=20&search=a%26b&account_id=42')
  const guard = latestRequestGuard()
  const first = guard.next(), second = guard.next()
  assert.equal(guard.isCurrent(first), false)
  assert.equal(guard.isCurrent(second), true)
  guard.next()
  assert.equal(guard.isCurrent(second), false)
})
test('conflict actions require structured account details and never infer IDs from an error message', () => {
  const details = { code: 'intelligence_check_target_conflict', target_group_id: 8, target_group_channel: 'codex', conflict_count: 1, conflicts: [{ account_id: 68876, account_name: 'Claude', channel: 'claude' }] }
  assert.deepEqual(intelligenceTargetConflict({ details }), details)
  for (const error of [null, new Error('1 enabled accounts belong to another channel'), { details: null }, { details: {...details, code: 'revision_conflict'} }, { details: {...details, conflicts: [{account_id: -1}]} }, { details: {...details, conflicts: []} }]) {
    assert.equal(intelligenceTargetConflict(error), null)
  }
})
test('a quota wait is labelled without promising a check time', () => {
  assert.equal(intelligenceScheduleLabel({ enabled: true, action: 'none', records: [], probe_phase: 'quota_wait', next_due_at: null }), '等待额度恢复')
  assert.equal(intelligenceScheduleLabel({ enabled: true, action: 'none', records: [] }), '')
})

test('history preserves legacy, empty and populated snapshots and sends UTC time bounds', () => {
  assert.equal(intelligenceGroupNames(undefined), '未记录')
  assert.equal(intelligenceGroupNames([]), '无分组')
  assert.equal(intelligenceGroupNames([{id:1, name:'旧分组'}, {id:2, name:''}]), '旧分组、#2')
  assert.equal(intelligencePolicyName({ groups: [] }), '全局配置')
  assert.equal(intelligencePolicyName({ rule_id:'rule-1', rule_name:'旧规则' }), '旧规则')
  assert.deepEqual(intelligenceTimeRange('2026-10-15T10:00:00+02:00', '2026-10-15T11:00:00+02:00'), { from:'2026-10-15T08:00:00.000Z', before:'2026-10-15T09:00:00.000Z' })
  assert.equal(intelligenceTimeRange('bad', ''), null)
  assert.equal(intelligenceTimeRange('2026-10-15T10:00:00Z', '2026-10-15T10:00:00Z'), null)
})

test('rule validation distinguishes omitted overrides, required matchers and enabled question subsets', () => {
  const rule = { id:'r1', name:' 套餐规则 ', channel:'codex', automatic:false, plan:' PLUS ' }
  const groups = [{id:1, channel:'codex'}, {id:2, channel:'claude'}]
  const questions = [{id:'on', enabled:true}, {id:'off', enabled:false}]
  const validate = patch => intelligenceRuleError({...rule, ...patch}, questions, groups)
  assert.equal(normalizeIntelligenceRule(rule).name, '套餐规则')
  assert.equal(normalizeIntelligenceRule(rule).plan, 'plus')
  assert.equal(validate({}), '')
  assert.equal(validate({ group_id:1, plan:undefined, question_ids:[] }), '')
  assert.match(validate({ plan:' ' }), /分组或套餐码/)
  assert.match(validate({ channel:'' }), /账号来源/)
  assert.match(validate({ action:'switch_group' }), /目标分组/)
  assert.match(validate({ action:'switch_group', target_group_id:2 }), /同来源/)
  assert.equal(validate({ action:'switch_group', target_group_id:1, question_ids:['off','on'] }), '')
  assert.match(validate({ question_ids:['off'] }), /至少启用/)
  assert.match(validate({ timeout_seconds:0 }), /1–3600/)
})
