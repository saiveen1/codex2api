import test from 'node:test'
import assert from 'node:assert/strict'
import { readFileSync } from 'node:fs'
import vm from 'node:vm'
import ts from 'typescript'

// Exercise the actual settings serializers without mounting the settings page.
const text = readFileSync(new URL('../pages/Settings.tsx', import.meta.url), 'utf8')
const source = ts.createSourceFile('Settings.tsx', text, ts.ScriptTarget.Latest, true, ts.ScriptKind.TSX)
const names = new Set(['CODEX_UA_STRING_KEYS', 'normalizeVersionText', 'parseCodexUserAgentConfig', 'serializeCodexUserAgentConfig'])
const selected = source.statements.filter(node => ts.isVariableStatement(node) && node.declarationList.declarations.some(d => ts.isIdentifier(d.name) && names.has(d.name.text)))
assert.equal(selected.length, names.size)
const code = ts.transpileModule(selected.map(node => node.getText(source)).join('\n'), { compilerOptions: { target: ts.ScriptTarget.ES2022 } }).outputText
const { parse, serialize } = vm.runInNewContext(`${code}\n({ parse: parseCodexUserAgentConfig, serialize: serializeCodexUserAgentConfig })`)

test('authorization OS matching defaults on and preserves an explicit off switch', () => {
  assert.equal(parse('{}').auto_match_os, true)
  const raw = { mode: 'pool', auto_match_os: false, pool_mix: { 'codex-tui': 20 }, account_kinds: { 12: 'codex-vscode' }, account_user_agents: { 11: 'manual/1.2.3' } }
  assert.deepEqual(JSON.parse(serialize(parse(JSON.stringify(raw)))), raw)
  const enabled = parse(JSON.stringify(raw))
  enabled.auto_match_os = true
  const saved = JSON.parse(serialize(enabled))
  assert.equal(saved.auto_match_os, undefined)
  assert.equal(parse(JSON.stringify(saved)).auto_match_os, true)
  assert.deepEqual(saved.account_user_agents, raw.account_user_agents)
  const combined = { ...raw, account_kinds: { 12: 'codex-tui@macos', 13: 'codex-desktop@windows' } }
  assert.deepEqual(JSON.parse(serialize(parse(JSON.stringify(combined)))), combined)
})

test('automatic OS matching switch is bound to the persisted setting', () => {
  assert.match(text, /checked=\{codexUserAgentConfig\.auto_match_os !== false\}/)
  assert.match(text, /onCheckedChange=\{\(checked\) => patchAndSaveCodexUserAgentConfig\(\{ auto_match_os: checked \}\)\}/)
  for (const language of ['zh', 'zh-TW', 'en']) {
    const locale = JSON.parse(readFileSync(new URL(`../locales/${language}.json`, import.meta.url), 'utf8'))
    assert.ok(locale.settings.codexUAAutoMatchOS)
    assert.ok(locale.settings.codexUAAutoMatchOSDesc)
  }
})
