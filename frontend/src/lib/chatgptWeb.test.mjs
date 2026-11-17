import { test } from 'node:test'
import assert from 'node:assert/strict'
import { readFileSync } from 'node:fs'

const source = path => readFileSync(new URL(path, import.meta.url), 'utf8')

test('web channel is explicit and survives key editing', () => {
  const keys = source('../pages/APIKeys.tsx')
  assert.match(keys, /key: "chatgpt_web"/)
  assert.match(keys, /limits\.upstream_channel === "chatgpt_web"/)
  assert.match(keys, /form\.upstreamChannel === "auto" \? undefined : form\.upstreamChannel/)
  assert.match(keys, /upstreamChannel: "auto"/)
})

test('web controls require explicit actions and executions are bounded', () => {
  const panel = source('../components/ChatGPTWebSettings.tsx')
  assert.match(panel, /e\.executions\.slice\(0, 50\)/)
  assert.match(panel, /onClick=\{\(\) => void action\(account\.id, !state\?\.enabled, false\)\}/)
  assert.match(panel, /disabled=\{busy \|\| !state\?\.enabled\}/)
  assert.match(panel, /image_quota_remaining \?\?/)
})

test('both studios constrain known web keys and keep native controls', () => {
  for (const page of ['../pages/ImageStudio.tsx', '../pages/ImageStudioPortal.tsx']) {
    const content = source(page)
    assert.match(content, /isWebKey/)
    assert.match(content, /'1:1', '3:2', '2:3', '16:9', '9:16'/)
    assert.match(content, /quality !== 'auto'/)
    assert.match(content, /option\.value === 'auto'/)
    assert.match(content, /upstreamChannelHint\.chatgpt_web/)
  }
})

test('web account guidance is discoverable in both key forms and the studio', () => {
  const keys = source('../pages/APIKeys.tsx')
  assert.match(keys, /value=\{createForm\.limits\.upstreamChannel\}/)
  assert.match(keys, /value=\{editForm\.limits\.upstreamChannel\}/)
  assert.match(keys, /value === "chatgpt_web" && \([\s\S]*?chatgptWebAutoHint[\s\S]*?to="\/settings#settings-codex-images"/)
  const studio = source('../pages/ImageStudio.tsx')
  assert.match(studio, /isWebKey && <div[^>]*>\s*<p>\{t\('chatgptWebAutoHint'\)\}<\/p>\s*<NavLink[^>]*to="\/settings#settings-codex-images"/)
  assert.match(source('../components/ChatGPTWebSettings.tsx'), /chatgptWebAutoHint/)
  for (const locale of ['en', 'zh', 'zh-TW']) {
    const messages = JSON.parse(source(`../locales/${locale}.json`))
    assert.match(messages.chatgptWebAutoHint, /30/)
    assert.ok(messages.chatgptWebManageAccounts)
    assert.doesNotMatch(messages.chatgptWeb.batchHint, /future members are not automatically enabled|后续新增成员不会自动启用|後續新增成員不會自動啟用/)
  }
})
