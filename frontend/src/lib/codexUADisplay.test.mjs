import assert from 'node:assert/strict'
import { readFileSync } from 'node:fs'
import test from 'node:test'
import i18next from 'i18next'
import { codexUADisplayText } from './codexUADisplay.ts'

const resources = Object.fromEntries(['zh', 'zh-TW', 'en'].map(lang => [lang, { translation: JSON.parse(readFileSync(new URL(`../locales/${lang}.json`, import.meta.url), 'utf8')) }]))
const i18n = i18next.createInstance()
await i18n.init({ resources, lng: 'zh', fallbackLng: 'zh', showSupportNotice: false })
const t = i18n.getFixedT('zh')

test('shows the OAuth OS and the currently assigned pool client separately', () => {
  const result = codexUADisplayText({ system: 'macos', system_source: 'oauth', client: 'codex-vscode', client_source: 'pool' }, t)
  assert.equal(t('accounts.uaDisplay.system', { value: result.system }), '系统：macOS（OAuth 匹配）')
  assert.equal(t('accounts.uaDisplay.client', { value: result.client }), '客户端：跟随全局（号池当前分配：VS Code 插件）')
})

test('overrides are not called pool matches and request-dependent results are not guessed', () => {
  assert.deepEqual(codexUADisplayText({ system: 'windows', system_source: 'preset', client: 'codex-exec', client_source: 'preset' }, t), { system: 'Windows（指定预设）', client: 'codex exec（指定预设）' })
  assert.deepEqual(codexUADisplayText({ system: '', system_source: 'request', client: '', client_source: 'request' }, t), { system: '随请求决定', client: '随请求决定' })
})

test('the same read-only summary is visible on the account row, card and editor', () => {
  const page = readFileSync(new URL('../pages/Accounts.tsx', import.meta.url), 'utf8')
  assert.equal(page.match(/<AccountUAStatus display=\{account\.codex_ua_display\}/g)?.length, 2)
  assert.match(page, /<AccountUAStatus display=\{editingAccount\.codex_ua_display\} detail/)
  for (const lang of Object.keys(resources)) assert.deepEqual(Object.keys(resources[lang].translation.accounts.uaDisplay).sort(), Object.keys(resources.zh.translation.accounts.uaDisplay).sort())
})
