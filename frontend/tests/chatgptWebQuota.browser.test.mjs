import assert from 'node:assert/strict'
import { after, before, test } from 'node:test'
import path from 'node:path'
import { fileURLToPath } from 'node:url'
import { mkdir, mkdtemp } from 'node:fs/promises'
import { tmpdir } from 'node:os'
import { build } from 'vite'
import react from '@vitejs/plugin-react'
import tailwindcss from '@tailwindcss/vite'
import { chromium } from 'playwright'

const root = fileURLToPath(new URL('../', import.meta.url))
let browser, script, css, artifactDir

before(async () => {
  artifactDir = process.env.IMAGE_QUOTA_TEST_ARTIFACT_DIR || await mkdtemp(path.join(tmpdir(), 'codex2api-web-quota-'))
  await mkdir(artifactDir, { recursive: true })
  const result = await build({ configFile: false, root, logLevel: 'silent', plugins: [react(), tailwindcss(), {
    name: 'web-quota-harness', enforce: 'pre',
    resolveId(id) { if (id === 'virtual:web-quota-harness') return '\0web-quota-harness' },
    load(id) {
      if (id !== '\0web-quota-harness') return
      return `import React from 'react'; import {createRoot} from 'react-dom/client';
        import Quota from ${JSON.stringify(path.join(root, 'src/components/ChatGPTWebAccountBadge.tsx'))};
        import {MemoryRouter} from 'react-router-dom';
        import ${JSON.stringify(path.join(root, 'src/i18n.ts'))}; import ${JSON.stringify(path.join(root, 'src/index.css'))};
        createRoot(document.getElementById('root')).render(React.createElement(MemoryRouter,null,
          React.createElement('main',{className:'p-4',onClick:()=>window.__rowClicks++},
            React.createElement(Quota,{accountID:7,accountName:'quota-fixture',state:window.__initialState,onStateChange:next=>window.__stateChanges.push(next)}))));`
    },
  }], resolve: { alias: { '@': path.join(root, 'src') } }, define: { 'process.env.NODE_ENV': '"production"' }, build: {
    write: false, minify: false, rollupOptions: { input: 'virtual:web-quota-harness' },
    lib: { entry: 'virtual:web-quota-harness', formats: ['iife'], name: 'WebQuotaHarness' },
  } })
  const bundle = Array.isArray(result) ? result[0] : result
  script = bundle.output.find(item => item.type === 'chunk').code
  css = bundle.output.filter(item => item.type === 'asset' && item.fileName.endsWith('.css')).map(item => item.source).join('\n')
  browser = await chromium.launch({ headless: true, executablePath: process.env.CHROMIUM_PATH || undefined, args: ['--no-sandbox'] })
})
after(async () => { await browser?.close() })

async function fixture(t, { remaining = null, serviceReady = true, enabled = true, refreshFail = false, recorded = false, reloadFail = false } = {}) {
  const page = await browser.newPage({ viewport: { width: 1280, height: 900 } })
  page.setDefaultTimeout(5000)
  const errors = []
  page.on('pageerror', error => errors.push(error.message))
  t.after(() => assert.deepEqual(errors, []))
  t.after(() => page.close())
  await page.route('http://quota.test/**', route => route.fulfill({ contentType: 'text/html', body: '<div id="root"></div>' }))
  await page.goto('http://quota.test/')
  await page.evaluate(({ remaining, serviceReady, enabled, refreshFail, recorded, reloadFail }) => {
    localStorage.setItem('lang', 'zh')
    let state = { account_id: 7, enabled, status: remaining === null ? 'unverified' : 'ready', capabilities: [], image_quota_remaining: remaining,
      observed_at: remaining === null ? null : '2026-11-16T08:00:00Z', quota_reset_at: remaining === null ? null : '2026-11-17T08:00:00Z' }
    window.__initialState = state
    window.__writes = []
    window.__stateChanges = []
    window.__rowClicks = 0
    window.__stateReads = 0
    window.fetch = async (url, options = {}) => {
      const pathname = new URL(String(url), location.origin).pathname
      const method = options.method || 'GET'
      let body, status = 200
      if (method !== 'GET') window.__writes.push({ pathname, method })
      if (pathname === '/api/admin/chatgpt-web' && method === 'GET') body = { enabled: serviceReady, configured: serviceReady, protocol_version: 'fixture', supported_models: ['gpt-image-2'] }
      else if (pathname === '/api/admin/chatgpt-web/accounts' && method === 'GET') {
        window.__stateReads++
        if (reloadFail && window.__stateReads > 1) throw new Error('Synthetic reload failure')
        body = { accounts: [state] }
      }
      else if (pathname === '/api/admin/chatgpt-web/accounts/7' && method === 'PUT') {
        state = { ...state, enabled: JSON.parse(options.body).enabled }; body = state
      }
      else if (pathname === '/api/admin/chatgpt-web/accounts/7/check' && method === 'POST') {
        if (refreshFail) {
          state = { ...state, status: 'unverified', last_check_at: '2026-11-16T12:00:00Z', last_check_stage: 'identity', last_check_error_code: 'token_expired', last_check_http_status: 401 }
          status = 422
          body = { error: 'Synthetic check failure', code: 'token_expired', stage: 'identity', upstream_status: 401, recorded, state: recorded ? state : { ...state, image_quota_remaining: 999 } }
        }
        else { state = { ...state, status: 'quota_exhausted', image_quota_remaining: 0, observed_at: '2026-11-16T12:00:00Z', quota_reset_at: '2026-11-17T12:00:00Z' }; body = state }
      } else throw new Error('Unexpected request: ' + method + ' ' + pathname)
      return new Response(JSON.stringify(body), { status, headers: { 'Content-Type': 'application/json' } })
    }
  }, { remaining, serviceReady, enabled, refreshFail, recorded, reloadFail })
  await page.addStyleTag({ content: css })
  await page.addScriptTag({ content: script })
  await page.getByRole('button', { name: '查看 quota-fixture 的网页生图额度', exact: true }).click()
  await page.getByRole('dialog').waitFor()
  await page.waitForFunction(() => document.querySelector('dl[aria-busy]')?.getAttribute('aria-busy') === 'false')
  return page
}

test('quota opens from saved state and only manual refresh checks upstream, preserving a zero result', async t => {
  const page = await fixture(t)
  const dialog = page.getByRole('dialog')
  assert.equal(await dialog.getByTestId('web-image-quota-remaining').textContent(), '未知')
  assert.deepEqual(await page.evaluate(() => window.__writes), [])
  await dialog.getByRole('button', { name: '刷新额度（不生成图片）', exact: true }).click()
  await page.waitForFunction(() => document.querySelector('[data-testid="web-image-quota-remaining"]')?.textContent === '0')
  assert.equal(await dialog.getByTestId('web-image-quota-remaining').textContent(), '0')
  const timestamps = await dialog.locator('dd').allTextContents()
  assert.equal(timestamps.slice(1).every(value => value !== '—' && value.length > 0), true)
  assert.deepEqual(await page.evaluate(() => window.__writes), [{ pathname: '/api/admin/chatgpt-web/accounts/7/check', method: 'POST' }])
  assert.equal(await page.evaluate(() => window.__rowClicks), 0, 'quota actions must not open the surrounding account detail')
  await page.screenshot({ path: path.join(artifactDir, 'web-quota-desktop.png'), fullPage: true })
  assert.equal(await page.evaluate(() => window.__stateChanges.at(-1).image_quota_remaining), 0)
  await page.setViewportSize({ width: 390, height: 844 })
  const bounds = await dialog.boundingBox()
  assert.equal(bounds.x >= 0 && bounds.x + bounds.width <= 391, true)
  await page.screenshot({ path: path.join(artifactDir, 'web-quota-mobile.png'), fullPage: true })
  await page.keyboard.press('Escape')
  assert.match(await page.getByRole('button', { name: '查看 quota-fixture 的网页生图额度', exact: true }).textContent(), /图片额度: 0/)
})

test('a failed quota refresh keeps the last known value and timestamp', async t => {
  const page = await fixture(t, { remaining: 12, refreshFail: true })
  const dialog = page.getByRole('dialog')
  const before = await dialog.locator('dd').allTextContents()
  await dialog.getByRole('button', { name: '刷新额度（不生成图片）', exact: true }).click()
  await dialog.getByRole('alert').waitFor()
  assert.deepEqual(await dialog.locator('dd').allTextContents(), before)
  assert.equal(await dialog.getByTestId('web-image-quota-remaining').textContent(), '12')
})

test('unconfigured service or disabled account never enables itself or submits a quota check', async t => {
  for (const options of [{ serviceReady: false }, { enabled: false }]) {
    const page = await fixture(t, options)
    const dialog = page.getByRole('dialog')
    assert.equal(await dialog.getByRole('button', { name: '刷新额度（不生成图片）', exact: true }).isDisabled(), true)
    assert.deepEqual(await page.evaluate(() => window.__writes), [])
    assert.equal(await dialog.getByRole('link', { name: '查看 Web 设置', exact: true }).isVisible(), true)
  }
})

test('explicit enable and disable only update saved state, without checking upstream', async t => {
  const page = await fixture(t, { remaining: 12, enabled: false, serviceReady: false })
  const dialog = page.getByRole('dialog')
  await dialog.getByRole('button', { name: '启用', exact: true }).click()
  await page.waitForFunction(() => window.__stateChanges.at(-1)?.enabled === true)
  assert.equal(await dialog.getByRole('button', { name: '启用', exact: true }).isDisabled(), true)
  await dialog.getByRole('button', { name: '停用', exact: true }).click()
  await page.waitForFunction(() => window.__stateChanges.at(-1)?.enabled === false)
  assert.deepEqual(await page.evaluate(() => window.__writes.map(item => item.method)), ['PUT', 'PUT'])
  assert.equal(await dialog.getByTestId('web-image-quota-remaining').textContent(), '12')
  await page.keyboard.press('Escape')
  assert.match(await page.getByRole('button', { name: '查看 quota-fixture 的网页生图额度', exact: true }).textContent(), /未启用/)
})

test('structured failures retain quota, show diagnostics and only trust recorded state', async t => {
  for (const recorded of [true, false]) {
    const page = await fixture(t, { remaining: 12, refreshFail: true, recorded })
    const dialog = page.getByRole('dialog')
    await dialog.getByRole('button', { name: '刷新额度（不生成图片）', exact: true }).click()
    await page.waitForFunction(() => document.querySelector('dl[aria-busy]')?.getAttribute('aria-busy') === 'false' && document.querySelector('[role=alert]'))
    assert.match(await dialog.getByRole('alert').textContent(), /Synthetic check failure.*code: token_expired.*stage: identity.*upstream_status: 401/)
    assert.match(await dialog.getByTestId('web-last-check').textContent(), /identity.*token_expired.*401/)
    assert.equal(await dialog.getByTestId('web-image-quota-remaining').textContent(), '12')
    assert.equal(await page.evaluate(() => window.__stateReads), recorded ? 1 : 2)
    assert.equal(await page.evaluate(() => window.__stateChanges.at(-1).status), 'unverified')
    await page.keyboard.press('Escape')
    assert.match(await page.getByRole('button', { name: '查看 quota-fixture 的网页生图额度', exact: true }).textContent(), /待检查/)
  }
})

test('snapshot reload failure never replaces the original check error or quota', async t => {
  const page = await fixture(t, { remaining: 12, refreshFail: true, reloadFail: true })
  const dialog = page.getByRole('dialog')
  await dialog.getByRole('button', { name: '刷新额度（不生成图片）', exact: true }).click()
  await page.waitForFunction(() => window.__stateReads === 2 && document.querySelector('dl[aria-busy]')?.getAttribute('aria-busy') === 'false')
  assert.match(await dialog.getByRole('alert').textContent(), /Synthetic check failure/)
  assert.doesNotMatch(await dialog.getByRole('alert').textContent(), /Synthetic reload failure/)
  assert.equal(await dialog.getByTestId('web-image-quota-remaining').textContent(), '12')
})
