import assert from 'node:assert/strict'
import { after, before, test } from 'node:test'
import { mkdir } from 'node:fs/promises'
import { fileURLToPath } from 'node:url'
import path from 'node:path'
import { build } from 'vite'
import react from '@vitejs/plugin-react'
import tailwindcss from '@tailwindcss/vite'
import { chromium } from 'playwright'

const root = fileURLToPath(new URL('../', import.meta.url))
let browser, script, css
before(async () => {
  const result = await build({
    configFile: false, root, logLevel: 'silent',
    plugins: [react(), tailwindcss(), {
      name: 'diagnostic-harness',
      resolveId(id) { if (id === 'virtual:diagnostic-harness') return '\0diagnostic-harness' },
      load(id) {
        if (id !== '\0diagnostic-harness') return
        return `import React from 'react'; import {createRoot} from 'react-dom/client';
          import {MemoryRouter, Route, Routes} from 'react-router-dom';
          import {DiagnosticDetailsButton, diagnosticContextSummary} from ${JSON.stringify(path.join(root, 'src/components/DiagnosticDetailsDialog.tsx'))};
          import Usage from ${JSON.stringify(path.join(root, 'src/pages/Usage.tsx'))};
          import APIKeys from ${JSON.stringify(path.join(root, 'src/pages/APIKeys.tsx'))};
          import PublicUsage from ${JSON.stringify(path.join(root, 'src/pages/APIKeyUsagePortal.tsx'))};
          import {ToastProvider} from ${JSON.stringify(path.join(root, 'src/components/ToastProvider.tsx'))};
          import {VisibleChannelsProvider} from ${JSON.stringify(path.join(root, 'src/visibleChannels.tsx'))};
          import {ThemeProvider} from ${JSON.stringify(path.join(root, 'src/hooks/useTheme.tsx'))};
          import {BrandingProvider} from ${JSON.stringify(path.join(root, 'src/branding.tsx'))};
          import ${JSON.stringify(path.join(root, 'src/i18n.ts'))};
          import ${JSON.stringify(path.join(root, 'src/index.css'))};
          const h = React.createElement;
          const content = window.__route ? h(Routes, null,
            h(Route, {path:'/usage', element:h(VisibleChannelsProvider, null, h(Usage))}),
            h(Route, {path:'/api-keys', element:h(APIKeys)}),
            h(Route, {path:'/key-usage/:view', element:h(PublicUsage)})) : h('main', {style:{padding:16}},
            h('p', {id:'row-summary'}, diagnosticContextSummary(window.__log.diagnostic_context, window.__log.error_message)),
            h(DiagnosticDetailsButton, {log:window.__log}));
          createRoot(document.getElementById('root')).render(h(MemoryRouter, {initialEntries:[window.__route || '/']},
            h(ThemeProvider, null, h(BrandingProvider, null, h(ToastProvider, null, content)))));`
      },
    }],
    resolve: { alias: { '@': path.join(root, 'src') } },
    define: { 'process.env.NODE_ENV': '"production"' },
    build: { write: false, minify: false, rollupOptions: { input: 'virtual:diagnostic-harness' }, lib: { entry: 'virtual:diagnostic-harness', formats: ['iife'], name: 'DiagnosticHarness' } },
  })
  const bundle = Array.isArray(result) ? result[0] : result
  script = bundle.output.find(item => item.type === 'chunk').code
  css = bundle.output.filter(item => item.type === 'asset' && item.fileName.endsWith('.css')).map(item => item.source).join('\n')
  browser = await chromium.launch({ headless: true, executablePath: process.env.CHROMIUM_PATH || undefined, args: ['--no-sandbox'] })
})
after(async () => { await browser?.close() })

async function fixture(t, context, mobile = false) {
  const page = await browser.newPage({ viewport: mobile ? { width: 390, height: 844 } : { width: 1280, height: 900 } })
  page.setDefaultTimeout(5000)
  t.after(() => page.close())
  const errors = []
  page.on('pageerror', error => errors.push(error.message))
  t.after(() => assert.deepEqual(errors, []))
  await page.route('http://diagnostic.test/**', route => route.fulfill({ contentType: 'text/html', body: '<!doctype html><html><head><meta name="viewport" content="width=device-width, initial-scale=1"></head><body><div id="root"></div></body></html>' }))
  await page.goto('http://diagnostic.test/')
  await page.evaluate(context => {
    window.__log = { id: 1, status_code: 400, error_message: 'HTTP 400', request_id: 'fixture-request', endpoint: '/v1/responses', diagnostic_context: context }
    Object.defineProperty(navigator, 'clipboard', { configurable: true, value: { writeText: async text => { window.__copied = text } } })
  }, context)
  await page.addStyleTag({ content: css })
  await page.addScriptTag({ content: script })
  await page.getByRole('button', { name: '诊断详情', exact: true }).focus()
  await page.keyboard.press('Enter')
  const dialog = page.getByRole('dialog')
  await dialog.waitFor()
  return { page, dialog }
}

const event = {
  schema: 2, event: 'attempt_outcome', stage: 'upstream_http', capture_status: 'complete', outcome: 'failure', outcome_status: 400,
  request_id: 'fixture-request', account_id: 7, proxy_id: 2, stream: true, websocket: false, retry_scheduled: false,
  request: { inspection: 'complete', bytes: 200, input_items: 5, fingerprint: 'ingress-fingerprint' },
  attempt: { index: 1, http_status: 400, protocol: 'http', request_encoding: 'zstd', request_bytes: 100, request_capture: 'decoded_transport', request: { inspection: 'complete', bytes: 210, input_items: 6, fingerprint: 'outbound-fingerprint' }, error: { detail: 'unavailable' } },
  error: { source: 'upstream_http', detail: 'generic', body_format: 'json', body_bytes: 24, message_present: true, message_class: 'generic_bad_request' },
}

test('diagnostic dialog distinguishes actual submitted context, final evidence and copies safe JSON', async t => {
  const raw = JSON.stringify(event)
  const { page, dialog } = await fixture(t, raw)
  assert.match(await page.locator('#row-summary').innerText(), /通用错误/)
  await dialog.getByText(/入站请求形状 \/ ingress request shape/).click()
  await dialog.getByText(/实发请求形状 \/ submitted upstream request shape/).click()
  await dialog.getByText(/安全错误证据 \/ safe error evidence/).click()
  assert.equal(await dialog.getByText('ingress-fingerprint', { exact: true }).isVisible(), true)
  assert.equal(await dialog.getByText('outbound-fingerprint', { exact: true }).isVisible(), true)
  assert.match(await dialog.innerText(), /上游错误说明为通用文本/)
  assert.doesNotMatch(await dialog.innerText(), /未知值 \/ unknown: (upstream_http|decoded_transport|complete|failure)/)
  await dialog.getByRole('button', { name: '复制安全 JSON' }).click()
  assert.equal(await page.evaluate(() => window.__copied), raw)
  if (process.env.DIAGNOSTIC_TEST_ARTIFACT_DIR) {
    await mkdir(process.env.DIAGNOSTIC_TEST_ARTIFACT_DIR, { recursive: true })
    await page.screenshot({ path: path.join(process.env.DIAGNOSTIC_TEST_ARTIFACT_DIR, 'diagnostic-desktop.png') })
  }
  await page.keyboard.press('Escape')
  await dialog.waitFor({ state: 'hidden' })
})

test('historical and malformed diagnostics explain their limitations without crashing', async t => {
  for (const [raw, expected] of [[undefined, /历史记录未采集诊断上下文/], ['broken JSON', /诊断上下文无效/], ['x'.repeat(25000), /诊断上下文过大/]]) {
    const { dialog } = await fixture(t, raw)
    assert.match(await dialog.innerText(), expected)
  }
})

test('mobile local error shows gateway evidence instead of missing attempt evidence', async t => {
  const local = { ...event, stage: 'local_or_gateway', capture_status: 'partial', attempt: { index: 0, request_bytes: -1, error: { detail: 'unavailable' } }, error: { source: 'downstream_response', detail: 'structured', message_present: true, message_class: 'message_omitted' } }
  const { page, dialog } = await fixture(t, JSON.stringify(local), true)
  await dialog.getByText(/安全错误证据 \/ safe error evidence/).click()
  assert.match(await dialog.innerText(), /网关曾提供错误消息/)
  assert.match(await dialog.innerText(), /诊断为部分采集/)
  assert.equal(await page.evaluate(() => document.documentElement.scrollWidth <= window.innerWidth), true)
  if (process.env.DIAGNOSTIC_TEST_ARTIFACT_DIR) {
    await page.screenshot({ path: path.join(process.env.DIAGNOSTIC_TEST_ARTIFACT_DIR, 'diagnostic-mobile.png') })
  }
})

const teamDecision = {
  reason: 'no_available_account', attempted: false, has_candidate: false, requested_model: 'fixture-model', effective_model: 'fixture-route',
  group_ids: [42], channel: 'codex', scoped: 29, counts: { unauthorized: 28, account_key_denied: 1 },
  samples: [{ account_id: 29, reason: 'account_key_denied' }],
}

async function pageFixture(t, route, width = 1280, { failCatalog = false } = {}) {
  const page = await browser.newPage({ viewport: { width, height: 900 }, reducedMotion: 'reduce' })
  page.setDefaultTimeout(5000)
  const errors = []
  page.on('pageerror', error => errors.push(error.message))
  t.after(async () => { await page.close(); assert.deepEqual(errors, []) })
  await page.route('http://diagnostic.test/**', request => request.fulfill({ contentType: 'text/html', body: '<!doctype html><html><head><meta name="viewport" content="width=device-width, initial-scale=1"></head><body><div id="root"></div></body></html>' }))
  await page.goto('http://diagnostic.test/')
  await page.evaluate(({ route, teamDecision, failCatalog }) => {
    window.__route = route
    window.__requests = []
    localStorage.setItem('lang', 'zh')
    localStorage.setItem('codex2api:usage:visible-columns', JSON.stringify({ error: false }))
    localStorage.setItem('codex2api_key_usage_api_key', 'sk-fixture')
    Object.defineProperty(navigator, 'clipboard', { configurable: true, value: { writeText: async text => { window.__copied = text } } })
    const key = { id: 10, name: 'Team demo Key', key: 'sk-fixture', enabled: true, status: 'active', quota_limit: 0, quota_used: 0, total_used: 0, allowed_group_ids: [42], limits: {}, created_at: '2026-01-01T00:00:00Z' }
    const timestamp = new Date().toISOString()
    const log = { id: 1, created_at: timestamp, endpoint: '/v1/responses', request_id: 'fixture-team-503', api_key_id: 10, model: 'fixture-model', status_code: 503, input_tokens: 0, output_tokens: 0, cached_tokens: 0, duration_ms: 0, error_message: 'private-evidence-fixture', diagnostic_context: JSON.stringify({ stage: 'dispatch', decision: teamDecision, downstream_http_status: 503, outcome_status: 503, outcome: 'failure', error: { source: 'downstream_response' } }) }
    let catalogChecks = 0
    window.fetch = async (url, options = {}) => {
      const path = new URL(String(url), window.location.href)
      window.__requests.push([path.pathname, path.search, options.method || 'GET'])
      let body = {}, status = 200
      switch (path.pathname) {
        case '/api/admin/usage/logs': body = { logs: [log], total: 1 }; break
        case '/api/admin/usage/stats': body = { total_requests: 1, today_requests: 1 }; break
        case '/api/admin/keys': body = { keys: [key] }; break
        case '/api/admin/models': body = { models: ['fixture-model'] }; break
        case '/api/admin/account-groups': body = { groups: [] }; break
        case '/api/admin/keys-concurrency': body = { concurrency: {} }; break
        case '/api/admin/prompt-filter/newapi-bindings': body = { bindings: [] }; break
        case '/api/admin/keys/10/model-diagnostics':
          catalogChecks += 1
          if (failCatalog && catalogChecks === 1) {
            await new Promise(resolve => { window.__releaseCatalog = resolve })
            status = 503
            break
          }
          body = {
            current: { reason: 'ok', group_ids: [42], channel: 'codex', scoped: 29, visible: 1, models: ['fixture-model'], time: timestamp },
            last_empty: failCatalog ? undefined : { reason: 'models_filtered', channel: 'codex', visible: 1, time: timestamp, request_id: 'fixture-empty-catalog' },
            last_empty_status: failCatalog ? 'expired' : 'recorded',
          }
          break
        case '/api/key-usage/summary': body = { key, usage: { summary: { requests: 1, error_count: 1 }, models: [], endpoints: [], windows: {}, recent_logs: [log], recent_logs_total: 1 } }; break
      }
      return new Response(JSON.stringify(body), { status, headers: { 'Content-Type': 'application/json' } })
    }
  }, { route, teamDecision, failCatalog })
  await page.addStyleTag({ content: css })
  await page.addScriptTag({ content: script })
  return page
}

test('Team 503 status opens admin diagnostics with the error column hidden on desktop and mobile', async t => {
  for (const width of [1280, 390]) {
    const page = await pageFixture(t, '/usage', width)
    const button = page.getByRole('button', { name: /HTTP 503.*未派发.*查看诊断详情/ })
    await button.waitFor()
    assert.equal(await page.locator('th').filter({ hasText: /错误摘要/ }).count(), 0)
    await button.focus()
    assert.equal(await button.evaluate(node => node.matches(':focus-visible')), true)
    await page.keyboard.press('Enter')
    const dialog = page.getByRole('dialog')
    await dialog.getByText('未派发：28 个账号鉴权不可用，1 个账号未授权给此 Key', { exact: true }).waitFor()
    assert.equal(await dialog.evaluate(node => getComputedStyle(node).animationName), 'none')
    await dialog.getByText(/候选计数与账号样本/).click()
    const sample = dialog.getByRole('link', { name: '账号 #29：未授权给此 Key' })
    assert.equal(await sample.getAttribute('href'), '/accounts?account_id=29')
    assert.equal(await dialog.evaluate(node => node.scrollWidth <= node.clientWidth), true)
    await dialog.getByRole('button', { name: '复制请求 ID' }).click()
    if (process.env.DIAGNOSTIC_TEST_ARTIFACT_DIR) {
      await mkdir(process.env.DIAGNOSTIC_TEST_ARTIFACT_DIR, { recursive: true })
      await page.screenshot({ path: path.join(process.env.DIAGNOSTIC_TEST_ARTIFACT_DIR, `team-dispatch-${width}.png`) })
    }
    assert.equal(await page.evaluate(() => window.__copied), 'fixture-team-503')
    await page.keyboard.press('Escape')
    await dialog.waitFor({ state: 'hidden' })
    await page.waitForFunction(() => document.activeElement?.getAttribute('aria-label')?.includes('HTTP 503'))
    await button.click()
    await dialog.getByRole('button', { name: '查看同一请求的记录' }).click()
    await page.waitForFunction(() => window.__requests.some(([path, query]) => path === '/api/admin/usage/logs' && new URLSearchParams(query).get('request_id') === 'fixture-team-503'))
    await button.click()
    await dialog.getByRole('link', { name: '检查此 Key 的当前模型目录' }).click()
    await page.getByRole('region', { name: '当前检查' }).getByText('本地目录有可见模型').waitFor()
    await page.getByRole('region', { name: '最近空目录' }).getByText('模型被 Key 的规则全部过滤').waitFor()
    assert.equal(await page.getByText('fixture-empty-catalog', { exact: true }).isVisible(), true)
  }
})

test('Key row model check uses GET, shows loading and failure, and distinguishes expired from current', async t => {
  const page = await pageFixture(t, '/api-keys', 1280, { failCatalog: true })
  await page.getByRole('button', { name: '检查模型目录', exact: true }).click()
  const dialog = page.getByRole('dialog')
  await dialog.getByRole('status').waitFor()
  assert.equal(await dialog.getByRole('button', { name: '重新检查' }).isDisabled(), true)
  await page.waitForFunction(() => typeof window.__releaseCatalog === 'function')
  await page.evaluate(() => window.__releaseCatalog())
  await dialog.getByRole('alert').waitFor()
  await dialog.getByRole('button', { name: '重试检查' }).click()
  await dialog.getByText('最近空目录快照已过期。').waitFor()
  assert.deepEqual(await page.evaluate(() => window.__requests.filter(([path]) => path.endsWith('/model-diagnostics'))), [
    ['/api/admin/keys/10/model-diagnostics', '', 'GET'], ['/api/admin/keys/10/model-diagnostics', '', 'GET'],
  ])
})

test('public Key usage renders the 503 without administrative diagnostic evidence or requests', async t => {
  const page = await pageFixture(t, '/key-usage/logs', 390)
  await page.getByRole('button', { name: '503', exact: true }).first().waitFor()
  assert.doesNotMatch(await page.locator('body').innerText(), /private-evidence-fixture|未授权给此 Key|fixture-team-503|模型目录/)
  assert.equal(await page.getByRole('dialog').count(), 0)
  assert.equal(await page.evaluate(() => window.__requests.some(([path]) => path.startsWith('/api/admin/'))), false)
})
