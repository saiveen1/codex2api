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
          import {DiagnosticDetailsButton, diagnosticContextSummary} from ${JSON.stringify(path.join(root, 'src/components/DiagnosticDetailsDialog.tsx'))};
          import ${JSON.stringify(path.join(root, 'src/index.css'))};
          createRoot(document.getElementById('root')).render(React.createElement('main', {style:{padding:16}},
            React.createElement('p', {id:'row-summary'}, diagnosticContextSummary(window.__log.diagnostic_context, window.__log.error_message)),
            React.createElement(DiagnosticDetailsButton, {log:window.__log})));`
      },
    }],
    resolve: { alias: { '@': path.join(root, 'src') } },
    define: { 'process.env.NODE_ENV': '"production"' },
    build: { write: false, minify: false, rollupOptions: { input: 'virtual:diagnostic-harness' }, lib: { entry: 'virtual:diagnostic-harness', formats: ['iife'], name: 'DiagnosticHarness' } },
  })
  const bundle = Array.isArray(result) ? result[0] : result
  script = bundle.output.find(item => item.type === 'chunk').code
  css = bundle.output.filter(item => item.type === 'asset' && item.fileName.endsWith('.css')).map(item => item.source).join('\n')
  browser = await chromium.launch({ headless: true })
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
  assert.match(await dialog.innerText(), /网关曾提供错误消息/)
  assert.match(await dialog.innerText(), /诊断为部分采集/)
  assert.equal(await page.evaluate(() => document.documentElement.scrollWidth <= window.innerWidth), true)
  if (process.env.DIAGNOSTIC_TEST_ARTIFACT_DIR) {
    await page.screenshot({ path: path.join(process.env.DIAGNOSTIC_TEST_ARTIFACT_DIR, 'diagnostic-mobile.png') })
  }
})
