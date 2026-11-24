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
    name: 'proxy-management-harness', enforce: 'pre',
    resolveId(id) { if (id === 'virtual:proxy-management') return '\0proxy-management' },
    load(id) {
      if (id !== '\0proxy-management') return
      return `import React from 'react'; import {createRoot} from 'react-dom/client';
        import Proxies from ${JSON.stringify(path.join(root, 'src/pages/Proxies.tsx'))};
        import {ToastProvider} from ${JSON.stringify(path.join(root, 'src/components/ToastProvider.tsx'))};
        import {MemoryRouter} from 'react-router-dom';
        import ${JSON.stringify(path.join(root, 'src/i18n.ts'))};
        import ${JSON.stringify(path.join(root, 'src/index.css'))};
        createRoot(document.getElementById('root')).render(React.createElement(MemoryRouter,null,React.createElement(ToastProvider,null,React.createElement(Proxies))));`
    },
  }], resolve: { alias: { '@': path.join(root, 'src') } }, define: { 'process.env.NODE_ENV': '"production"' },
    build: { write: false, minify: false, rollupOptions: { input: 'virtual:proxy-management' }, lib: { entry: 'virtual:proxy-management', formats: ['iife'], name: 'ProxyManagementHarness' } },
  })
  const bundle = Array.isArray(result) ? result[0] : result
  script = bundle.output.find(item => item.type === 'chunk').code
  css = bundle.output.filter(item => item.type === 'asset' && item.fileName.endsWith('.css')).map(item => item.source).join('\n')
  browser = await chromium.launch({ headless: true, executablePath: process.env.CHROMIUM_PATH || undefined, args: ['--no-sandbox'] })
})
after(async () => { await browser?.close() })

async function fixture(t, empty = false) {
  const page = await browser.newPage({ viewport: { width: 1440, height: 1000 } })
  page.setDefaultTimeout(5_000)
  const errors = []
  page.on('pageerror', error => errors.push(error.message))
  t.after(() => assert.deepEqual(errors, []))
  t.after(() => page.close())
  await page.route('http://proxy-management.test/**', route => route.fulfill({ contentType: 'text/html', body: '<div id="root"></div>' }))
  await page.goto('http://proxy-management.test/')
  await page.evaluate(empty => {
    localStorage.setItem('lang', 'zh')
    window.requests = []
    window.fetch = async (url, options = {}) => {
      const u = new URL(String(url), location.origin)
      const method = options.method || 'GET'
      window.requests.push({ path: u.pathname, query: u.search, method })
      if (method !== 'GET') throw new Error('Unexpected production mutation in read-only fixture')
      let body = {}
      if (u.pathname.endsWith('/proxies')) body = { proxies: [
        { id: 1, label: 'Shared bound node', url: 'http://proxy-one.example:8080', enabled: true, exclusive: false, test_status: 'untested', bound_count: empty ? 0 : 2 },
        { id: 2, label: 'Free node', url: 'http://proxy-two.example:8080', enabled: true, exclusive: true, test_status: 'untested', bound_count: 0 },
        { id: 3, label: 'Disabled bound node', url: 'http://proxy-three.example:8080', enabled: false, exclusive: true, test_status: 'untested', bound_count: empty ? 0 : 1 },
      ] }
      if (u.pathname.endsWith('/settings')) body = { proxy_pool_enabled: false }
      if (u.pathname.endsWith('/profiles')) body = { profiles: [] }
      if (u.pathname.endsWith('/proxy-subscriptions')) body = { engine: { binary_path: '', auto_install: true, controller_port: 19090, port_start: 20000 }, subscriptions: [], nodes: [], running: false, version: '', last_error: '', operation: { running: false } }
      if (u.pathname.endsWith('/accounts')) body = { accounts: [{ id: 11, email: 'synthetic@example.invalid', proxy_url: 'http://proxy-one.example:8080' }], total: 1 }
      return new Response(JSON.stringify(body), { headers: { 'Content-Type': 'application/json' } })
    }
  }, empty)
  await page.addStyleTag({ content: css })
  await page.addScriptTag({ content: script })
  await page.getByRole('button', { name: /^已绑定账号/ }).waitFor()
  return page
}

test('bound statistic is clickable and filters proxies, including disabled bindings', async t => {
  const page = await fixture(t)
  const search = page.getByPlaceholder(/搜索代理 URL/)
  await search.fill('no matches')
  const tile = page.getByRole('button', { name: /^已绑定账号/ })
  assert.match(await tile.innerText(), /3/)
  await tile.click()
  assert.equal(await tile.getAttribute('aria-pressed'), 'true')
  assert.equal(await search.inputValue(), '')
  const table = page.getByRole('table')
  await table.getByText('Shared bound node', { exact: true }).waitFor()
  assert.equal(await table.getByText('Disabled bound node', { exact: true }).count(), 1)
  assert.equal(await table.getByText('Free node', { exact: true }).count(), 0)
  const boundRow = table.getByRole('row').filter({ hasText: 'Shared bound node' })
  await boundRow.getByRole('button').filter({ hasText: /^2$/ }).click()
  await page.getByRole('dialog').getByText('synthetic@example.invalid', { exact: true }).waitFor()
  assert.equal(await page.evaluate(() => window.requests.filter(item => item.method !== 'GET').length), 0)
})

test('bound filter remains usable with no bound accounts', async t => {
  const page = await fixture(t, true)
  const tile = page.getByRole('button', { name: /^已绑定账号/ })
  await tile.click()
  assert.equal(await tile.getAttribute('aria-pressed'), 'true')
  assert.equal(await page.getByRole('table').count(), 0)
  assert.equal(await page.getByText('Free node', { exact: true }).count(), 0)
})

test('access filter treats a reserved zero-binding proxy as exclusive, not shared', async t => {
  const page = await fixture(t)
  const table = page.getByRole('table')
  const filter = page.locator('button[aria-haspopup="listbox"]').filter({ hasText: /全部用途|专用|共享/ })
  await filter.click()
  await page.getByRole('option', { name: '专用', exact: true }).click()
  await table.getByText('Free node', { exact: true }).waitFor()
  assert.equal(await table.getByText('Shared bound node', { exact: true }).count(), 0)
  await filter.click()
  await page.getByRole('option', { name: '共享', exact: true }).click()
  await table.getByText('Shared bound node', { exact: true }).waitFor()
  assert.equal(await table.getByText('Free node', { exact: true }).count(), 0)
})
