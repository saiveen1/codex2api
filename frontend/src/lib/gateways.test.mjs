import { test } from 'node:test'
import assert from 'node:assert/strict'
import { readFileSync } from 'node:fs'

const source = path => readFileSync(new URL(`../${path}`, import.meta.url), 'utf8')

test('gateway sidebar and link tabs lead to the reused panels, with Web as default', () => {
  assert.match(source('components/Layout.tsx'), /to: '\/gateways', labelKey: 'nav\.gateways'/)
  const app = source('App.tsx')
  assert.match(app, /path="\/gateways\/:tab\?" element=\{<Gateways/)
  const page = source('pages/Gateways.tsx')
  assert.match(page, /tab = 'web'/)
  for (const path of ['/gateways', '/gateways/oauth', '/gateways/subscriptions']) assert.ok(page.includes(`to: '${path}'`))
  assert.match(page, /<NavLink[^>]*to=\{to\} end/)
  assert.match(page, /tab === 'web' && <ChatGPTWebSettings/)
  assert.match(page, /oauthActive \|\| hasOpenedOAuth[\s\S]*?hidden=\{!oauthActive\}[\s\S]*?<OAuthGateway embedded/)
  assert.match(page, /tab === 'subscriptions' && [\s\S]*?<ProxySubscriptions/)
})

test('legacy OAuth fragments and proxy and settings entry points are preserved', () => {
  const app = source('App.tsx')
  assert.match(app, /path="\/oauth-gateway" element=\{<LegacyOAuthGateway/)
  assert.match(app, /<Navigate replace to=\{\{ pathname: '\/gateways\/oauth', search, hash \}\}/)
  const oauth = source('pages/OAuthGateway.tsx')
  for (const id of ['oauth-prepare', 'oauth-jobs', 'oauth-recovery']) assert.ok(oauth.includes(`href="#${id}"`))
  assert.match(oauth, /active && hash && anchorReady.*scrollIntoView\(\)/)
  assert.match(source('components/OAuthJobs.tsx'), /id="oauth-jobs"/)
  const proxies = source('pages/Proxies.tsx')
  assert.match(proxies, /<Link to="\/gateways\/subscriptions"/)
  assert.doesNotMatch(proxies, /<ProxySubscriptions\b/)
  const settings = source('pages/Settings.tsx')
  assert.match(settings, /'settings-codex-images': 'codex'/)
  assert.match(settings, /id="settings-codex-images"[^>]*>[\s\S]*?<Link to="\/gateways"[\s\S]*?codex_images_main_model/)
  assert.doesNotMatch(settings, /<ChatGPTWebSettings\b/)
})

test('Web advertises the same key for chat and images and displays models reported by the backend', () => {
  assert.match(source('api.ts'), /supported_chat_models\?: string\[\]/)
  const panel = source('components/ChatGPTWebSettings.tsx')
  assert.match(panel, /deployment\?\.supported_models \?\? \[\].*deployment\?\.supported_chat_models \?\? \[\]/)
  assert.match(panel, /supportedModels\.join\(/)
  for (const locale of ['en', 'zh', 'zh-TW']) {
    const messages = JSON.parse(source(`locales/${locale}.json`))
    assert.ok(messages.nav.gateways)
    assert.equal(messages.gateways.web, 'Web')
    assert.match(messages.gateways.subscriptions, /Mihomo/)
    assert.match(messages.chatgptWeb.guide, /chat requests|对话|對話/)
    assert.match(messages.apiKeys.limits.upstreamChannelHint.chatgpt_web, /chat and image generation|对话和生图|對話和生圖/)
  }
})
