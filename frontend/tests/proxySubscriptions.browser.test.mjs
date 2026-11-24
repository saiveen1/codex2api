import assert from 'node:assert/strict'
import { after, before, test } from 'node:test'
import path from 'node:path'
import { fileURLToPath } from 'node:url'
import { build } from 'vite'
import react from '@vitejs/plugin-react'
import { chromium } from 'playwright'

const root = fileURLToPath(new URL('../', import.meta.url))
let browser, script
before(async () => {
  const result = await build({ configFile: false, root, logLevel: 'silent', plugins: [react(), {
    name: 'proxy-subscription-harness', enforce: 'pre',
    resolveId(id) { if (id === 'virtual:proxy-harness') return '\0proxy-harness' },
    load(id) { if (id !== '\0proxy-harness') return; return `import React from 'react'; import {createRoot} from 'react-dom/client';
      import ProxySubscriptions from ${JSON.stringify(path.join(root, 'src/components/ProxySubscriptions.tsx'))};
      import ProxyAccessModal from ${JSON.stringify(path.join(root, 'src/components/ProxyAccessModal.tsx'))};
      import ${JSON.stringify(path.join(root, 'src/i18n.ts'))};
      function App(){ const [open,setOpen]=React.useState(true); return React.createElement(React.Fragment,null,
        React.createElement(ProxySubscriptions,{onChanged:()=>{window.poolReloaded=(window.poolReloaded||0)+1}}),
        React.createElement(ProxyAccessModal,{proxy:open?{id:7,url:'http://proxy.test:20007',exclusive:true,allowed_account_ids:[11],bound_count:0}:null,onClose:()=>setOpen(false),onSaved:()=>{}})) }
      createRoot(document.getElementById('root')).render(React.createElement(App));` },
  }], resolve: { alias: { '@': path.join(root, 'src') } }, define: { 'process.env.NODE_ENV': '"production"' }, build: { write: false, minify: false, rollupOptions: { input: 'virtual:proxy-harness' }, lib: { entry: 'virtual:proxy-harness', formats: ['iife'], name: 'ProxyHarness' } } })
  script = (Array.isArray(result) ? result[0] : result).output.find(item => item.type === 'chunk').code
  browser = await chromium.launch({ headless: true, executablePath: process.env.CHROMIUM_PATH || undefined, args: ['--no-sandbox'] })
})
after(async () => { await browser?.close() })

test('subscription edits preserve a masked URL and regex fields; access keeps selections across pages', async t => {
  const page = await browser.newPage()
  t.after(() => page.close())
  await page.route('http://proxy-ui.test/**', route => route.fulfill({ contentType: 'text/html', body: '<div id="root"></div>' }))
  await page.goto('http://proxy-ui.test/')
  await page.evaluate(() => {
    localStorage.setItem('lang', 'en')
    window.requests = []
    window.fetch = async (url, options = {}) => {
      const u = new URL(String(url), location.origin)
      const method = options.method || 'GET'
      if (method !== 'GET') window.requests.push({ path: u.pathname, method, body: JSON.parse(options.body || '{}') })
      let body = {}
      if (u.pathname.endsWith('/proxy-subscriptions')) body = { engine: { binary_path: '', auto_install: true, controller_port: 19090, port_start: 20000 }, subscriptions: [{ id: '3', name: 'Synthetic', url: '', url_set: true, enabled: true, include_regex: 'US', exclude_regex: 'HK', dedupe_regex: '(.*)', prefer_regex: 'direct', refresh_minutes: 60, total_nodes: 2, kept_nodes: 1 }], nodes: [], running: false, version: '', last_error: '', operation: { running: false, stage: '', error: '', started_at: '', finished_at: '' } }
      if (u.pathname.endsWith('/accounts')) { const p = Number(u.searchParams.get('page')); body = { total: 21, accounts: [{ id: p === 1 ? 11 : 22, email: `account${p}@example.invalid`, name: '' }] } }
      if (u.searchParams.get('proxy_filter') === 'this') body = { total: 0, accounts: [] }
      return new Response(JSON.stringify(body), { headers: { 'Content-Type': 'application/json' } })
    }
  })
  await page.addScriptTag({ content: script })
  const dialog = page.getByRole('dialog')
  await dialog.getByText('account1@example.invalid').waitFor()
  assert.equal(await dialog.getByRole('checkbox').nth(1).isChecked(), true)
  await dialog.getByRole('button', { name: 'Next' }).click()
  await dialog.getByText('account2@example.invalid').waitFor()
  await dialog.getByRole('checkbox').nth(1).check()
  await dialog.getByRole('button', { name: 'Save' }).click()
  const access = await page.evaluate(() => window.requests.find(r => r.path.endsWith('/7/access')))
  assert.deepEqual(access.body.allowed_account_ids.sort((a, b) => a - b), [11, 22])
  await page.keyboard.press('Escape')
  await page.locator('summary').click()
  await page.getByRole('button', { name: 'Edit', exact: true }).click()
  const form = page.locator('form')
  assert.equal(await form.locator('input[type=password]').inputValue(), '')
  assert.equal(await form.getByLabel('Deduplicate regex').inputValue(), '(.*)')
  await form.getByRole('button', { name: 'Save' }).click()
  await page.waitForFunction(() => window.requests.some(r => r.method === 'PATCH' && r.path.endsWith('/proxy-subscriptions/3')))
  const saved = await page.evaluate(() => window.requests.find(r => r.path.endsWith('/proxy-subscriptions/3')))
  assert.equal(saved.body.url, '')
  assert.equal(saved.body.exclude_regex, 'HK')
  assert.equal(saved.body.prefer_regex, 'direct')
})
