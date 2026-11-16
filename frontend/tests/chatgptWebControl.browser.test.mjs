import assert from 'node:assert/strict'
import { after, before, test } from 'node:test'
import path from 'node:path'
import { fileURLToPath } from 'node:url'
import { build } from 'vite'
import react from '@vitejs/plugin-react'
import tailwindcss from '@tailwindcss/vite'
import { chromium } from 'playwright'

const root = fileURLToPath(new URL('../', import.meta.url))
let browser, script, css
before(async () => {
  const result = await build({ configFile: false, root, logLevel: 'silent', plugins: [react(), tailwindcss(), {
    name: 'web-control-harness', enforce: 'pre',
    resolveId(id) { if (id === 'virtual:web-control') return '\0web-control' },
    load(id) {
      if (id !== '\0web-control') return
      return `import React from 'react'; import {createRoot} from 'react-dom/client';
        import Control from ${JSON.stringify(path.join(root, 'src/components/ChatGPTWebServiceControl.tsx'))};
        import ${JSON.stringify(path.join(root, 'src/i18n.ts'))};
        import ${JSON.stringify(path.join(root, 'src/index.css'))};
        function Harness() { const [deployment,setDeployment]=React.useState(window.initial);
          return React.createElement(Control,{deployment,onChange:setDeployment}); }
        createRoot(document.getElementById('root')).render(React.createElement(Harness));`
    },
  }], resolve: { alias: { '@': path.join(root, 'src') } }, define: { 'process.env.NODE_ENV': '"production"' },
    build: { write: false, minify: false, rollupOptions: { input: 'virtual:web-control' }, lib: { entry: 'virtual:web-control', formats: ['iife'], name: 'WebControlHarness' } },
  })
  const bundle = Array.isArray(result) ? result[0] : result
  script = bundle.output.find(item => item.type === 'chunk').code
  css = bundle.output.filter(item => item.type === 'asset' && item.fileName.endsWith('.css')).map(item => item.source).join('\n')
  browser = await chromium.launch({ headless: true, executablePath: process.env.CHROMIUM_PATH || undefined, args: ['--no-sandbox'] })
})
after(async () => { await browser?.close() })

async function fixture(t, options = {}) {
  const page = await browser.newPage({ viewport: { width: 1000, height: 800 } })
  page.setDefaultTimeout(5000)
  const errors = []
  page.on('pageerror', error => errors.push(error.message))
  t.after(() => assert.deepEqual(errors, []))
  t.after(() => page.close())
  await page.route('http://web-control.test/**', route => route.fulfill({ contentType: 'text/html', body: '<main id="root"></main>' }))
  await page.goto('http://web-control.test/')
  await page.evaluate(({ mode = 'managed', enabled = false, available = true, stage = 'stopped', fail = false } = {}) => {
    localStorage.setItem('lang', 'zh')
    let current = { enabled, configured: enabled, protocol_version: '1', supported_models: ['gpt-image-2'], control: {
      config: { enabled, mode, worker_url: mode === 'external' ? 'http://127.0.0.1:8091' : '', timeout_seconds: 180 },
      secret_set: mode === 'external', available, running: enabled && mode === 'managed', stage, last_error: '',
    } }
    window.initial = current
    window.writes = []
    window.reads = 0
    window.fetch = async (url, init = {}) => {
      const pathname = new URL(String(url), location.origin).pathname
      if ((init.method || 'GET') === 'GET' && pathname === '/api/admin/chatgpt-web') {
        window.reads++
        return new Response(JSON.stringify(current), { headers: { 'Content-Type': 'application/json' } })
      }
      if (init.method !== 'PUT' || pathname !== '/api/admin/chatgpt-web/config') throw new Error('Unexpected request')
      const config = JSON.parse(init.body)
      window.writes.push(config)
      if (fail) return new Response(JSON.stringify({ error: 'Synthetic startup failure' }), { status: 400, headers: { 'Content-Type': 'application/json' } })
      current = { ...current, enabled: config.enabled, configured: config.enabled, control: { ...current.control,
        config: { ...config, worker_secret: '' }, secret_set: true, running: config.enabled && config.mode === 'managed',
        stage: config.enabled ? config.mode === 'managed' ? 'running' : 'external' : 'stopped',
      } }
      return new Response(JSON.stringify(current), { headers: { 'Content-Type': 'application/json' } })
    }
  }, options)
  await page.addStyleTag({ content: css })
  await page.addScriptTag({ content: script })
  await page.getByRole('button', { name: '保存并启用', exact: true }).waitFor()
  return page
}

test('managed service enables without URL or secret and disables using saved settings', async t => {
  const page = await fixture(t)
  assert.deepEqual(await page.evaluate(() => window.writes), [])
  assert.equal(await page.locator('input[type=password]').count(), 0)
  await page.getByLabel('请求超时（秒，10–600）').fill('45')
  await page.getByRole('button', { name: '保存并启用', exact: true }).click()
  await page.getByText('内置服务运行中', { exact: true }).waitFor()
  assert.equal(await page.getByLabel('运行方式', { exact: true }).isDisabled(), true)
  await page.getByLabel('请求超时（秒，10–600）').fill('999')
  await page.getByRole('button', { name: '停用服务', exact: true }).click()
  await page.getByText('已停止', { exact: true }).waitFor()
  const writes = await page.evaluate(() => window.writes)
  assert.equal(writes.length, 2)
  assert.deepEqual(writes.map(x => [x.enabled, x.timeout_seconds, x.worker_url, x.worker_secret]), [[true, 45, '', ''], [false, 45, '', '']])
  await page.setViewportSize({ width: 390, height: 844 })
  assert.equal(await page.evaluate(() => document.documentElement.scrollWidth <= window.innerWidth), true)
})

test('external saved secret stays blank and no worker process is claimed', async t => {
  const page = await fixture(t, { mode: 'external' })
  assert.equal(await page.getByLabel('Worker 密钥', { exact: true }).inputValue(), '')
  await page.getByRole('button', { name: '保存并启用', exact: true }).click()
  await page.getByText('外部服务（未验证连接）', { exact: true }).waitFor()
  assert.equal((await page.evaluate(() => window.writes))[0].worker_secret, '')
  assert.equal(await page.getByLabel('Worker 密钥', { exact: true }).inputValue(), '')
})

test('switching from managed to external requires an actual external secret', async t => {
  const page = await fixture(t)
  await page.getByLabel('运行方式', { exact: true }).selectOption('external')
  assert.equal(await page.getByLabel('Worker 密钥', { exact: true }).getAttribute('required'), '')
  await page.getByLabel('Worker 私有地址', { exact: true }).fill('http://127.0.0.1:8091')
  await page.getByLabel('Worker 密钥', { exact: true }).fill('synthetic-external-secret-1234')
  await page.getByRole('button', { name: '保存并启用', exact: true }).click()
  await page.getByText('外部服务（未验证连接）', { exact: true }).waitFor()
  assert.equal((await page.evaluate(() => window.writes))[0].worker_secret, 'synthetic-external-secret-1234')
  assert.equal(await page.getByLabel('Worker 密钥', { exact: true }).inputValue(), '')
})

test('startup failure is visible and only re-reads state, never retries save', async t => {
  const page = await fixture(t, { fail: true })
  await page.getByRole('button', { name: '保存并启用', exact: true }).click()
  await page.getByRole('alert').waitFor()
  assert.match(await page.getByRole('alert').textContent(), /Synthetic startup failure/)
  assert.equal(await page.evaluate(() => window.writes.length), 1)
  assert.equal(await page.evaluate(() => window.reads), 1)
})

test('missing bundled runtime and draining state cannot start another worker', async t => {
  for (const options of [{ available: false }, { stage: 'draining' }]) {
    const page = await fixture(t, options)
    assert.equal(await page.getByRole('button', { name: '保存并启用', exact: true }).isDisabled(), true)
    assert.deepEqual(await page.evaluate(() => window.writes), [])
  }
})
