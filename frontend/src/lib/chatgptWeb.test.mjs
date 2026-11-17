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
  assert.match(keys, /webMode=\{createForm\.limits\.webSchedulingMode\}/)
  assert.match(keys, /webMode=\{editForm\.limits\.webSchedulingMode\}/)
  assert.match(keys, /webSchedulingMode: limits\.web_scheduling_mode \?\? "fill_first"/)
  assert.match(keys, /web_scheduling_mode: form\.upstreamChannel === "chatgpt_web" \? form\.webSchedulingMode : undefined/)
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

test('studio Web model choice and submission use key-scoped aliases without rewriting', () => {
  for (const page of ['../pages/ImageStudio.tsx', '../pages/ImageStudioPortal.tsx']) {
    const content = source(page)
    assert.match(content, /options=\{isWebKey \? webModels : IMAGE_MODELS\}/)
    assert.match(content, /webModels\.some\(option => option\.value === model\)/)
    assert.doesNotMatch(content, /setModel\('gpt-image-2'\)\s*\n\s*setSize\(current => webImageParams/)
  }
  assert.match(source('../pages/ImageStudio.tsx'), /webModels\.some\(option => option\.value === payload\.model\)/)
  assert.match(source('../pages/ImageStudioPortal.tsx'), /limits: result\.key\.limits/)
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
    assert.doesNotMatch(messages.chatgptWebAutoHint, /30 minutes|30 分钟|30 分鐘/)
    assert.match(messages.chatgptWebAutoHint, /automatic checks|自动检查|自動檢查/)
    assert.match(messages.chatgptWeb.guide, /manual|手动|手動/)
    assert.ok(messages.chatgptWebManageAccounts)
    for (const key of ['label', 'fillFirst', 'roundRobin', 'hint']) assert.ok(messages.chatgptWebScheduling[key])
    assert.doesNotMatch(messages.chatgptWeb.batchHint, /future members are not automatically enabled|后续新增成员不会自动启用|後續新增成員不會自動啟用/)
  }
})
