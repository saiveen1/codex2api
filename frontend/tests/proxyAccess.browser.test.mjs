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
    name: 'proxy-access-harness', enforce: 'pre',
    resolveId(id) { if (id === 'virtual:proxy-access') return '\0proxy-access' },
    load(id) {
      if (id !== '\0proxy-access') return
      return `import React from 'react'; import {createRoot} from 'react-dom/client';
        import ProxyAccessModal from ${JSON.stringify(path.join(root, 'src/components/ProxyAccessModal.tsx'))};
        import ${JSON.stringify(path.join(root, 'src/i18n.ts'))};
        function App() { const [proxy,setProxy]=React.useState(window.fixtureProxy); window.setFixtureProxy=setProxy;
          return React.createElement(ProxyAccessModal,{proxy,onClose:()=>setProxy(null),onSaved:()=>window.saved++}); }
        createRoot(document.getElementById('root')).render(React.createElement(App));`
    },
  }], resolve: { alias: { '@': path.join(root, 'src') } }, define: { 'process.env.NODE_ENV': '"production"' },
    build: { write: false, minify: false, rollupOptions: { input: 'virtual:proxy-access' }, lib: { entry: 'virtual:proxy-access', formats: ['iife'], name: 'ProxyAccessHarness' } },
  })
  script = (Array.isArray(result) ? result[0] : result).output.find(item => item.type === 'chunk').code
  browser = await chromium.launch({ headless: true, executablePath: process.env.CHROMIUM_PATH || undefined, args: ['--no-sandbox'] })
})
after(async () => { await browser?.close() })

async function fixture(t, options = {}) {
  const page = await browser.newPage()
  page.setDefaultTimeout(5000)
  const errors = []
  page.on('pageerror', error => errors.push(error.message))
  t.after(() => assert.deepEqual(errors, []))
  t.after(() => page.close())
  await page.route('http://proxy-access.test/**', route => route.fulfill({ contentType: 'text/html', body: '<div id="root"></div>' }))
  await page.goto('http://proxy-access.test/')
  await page.evaluate(({ count = 1, exclusive = false, allowed = [], failPage = 0, hold = false, incomplete = false }) => {
    localStorage.setItem('lang', 'en')
    window.fixtureProxy = { id: 7, url: 'http://proxy.test:20007', exclusive, allowed_account_ids: allowed, bound_count: 0 }
    window.boundRequests = []; window.writes = []; window.saved = 0
    window.failPage = failPage; window.holdBound = hold; window.incomplete = incomplete
    window.fetch = async (url, options = {}) => {
      const u = new URL(String(url), location.origin)
      const method = options.method || 'GET'
      if (method !== 'GET') {
        if (method !== 'PATCH' || !u.pathname.endsWith('/access')) throw new Error('Unexpected write')
        window.writes.push({ method, path: u.pathname, body: JSON.parse(options.body) })
        return new Response('{}', { headers: { 'Content-Type': 'application/json' } })
      }
      if (!u.pathname.endsWith('/accounts')) throw new Error('Unexpected read')
      const p = Number(u.searchParams.get('page'))
      const size = Number(u.searchParams.get('page_size'))
      let accounts, total
      if (u.searchParams.get('proxy_filter') === 'this') {
        if (u.searchParams.get('sort') !== 'id' || u.searchParams.get('order') !== 'asc' || size !== 100 || !u.searchParams.get('proxy_url')) throw new Error('Incorrect binding filter')
        window.boundRequests.push({ page: p, url: u.searchParams.get('proxy_url') })
        const n = u.searchParams.get('proxy_url').endsWith('20008') ? 2 : count
        total = n
        accounts = Array.from({ length: Math.max(0, Math.min(size, n - (p - 1) * size)) }, (_, i) => ({ id: 10000 + (p - 1) * size + i, email: `bound${10000 + (p - 1) * size + i}@example.invalid` }))
        if (window.incomplete && p === 2) accounts = []
        if (window.failPage === p) return new Response('{"error":"Synthetic binding read failed"}', { status: 500, headers: { 'Content-Type': 'application/json' } })
        if (window.holdBound) await new Promise(resolve => { window.releaseBound = resolve }) // Deliberately ignore abort to test late responses.
      } else {
        total = 40
        accounts = Array.from({ length: 20 }, (_, i) => ({ id: 200 + (p - 1) * 20 + i, email: `other${200 + (p - 1) * 20 + i}@example.invalid` }))
      }
      return new Response(JSON.stringify({ accounts, total, page: p, page_size: size }), { headers: { 'Content-Type': 'application/json' } })
    }
  }, options)
  await page.addScriptTag({ content: script })
  await page.getByRole('dialog').waitFor()
  return page
}
const exclusive = page => page.getByRole('checkbox', { name: 'Exclusive', exact: true })
const save = page => page.getByRole('button', { name: 'Save', exact: true })
const bound = (page, id = 10000) => page.getByRole('checkbox', { name: `bound${id}@example.invalid #${id}`, exact: true })
const other = (page, id = 200) => page.getByRole('checkbox', { name: `other${id}@example.invalid #${id}`, exact: true })

test('a bound account automatically makes the proxy exclusive even when the cached count is zero', async t => {
  const page = await fixture(t)
  await page.getByText('1 selected', { exact: true }).waitFor()
  assert.equal(await bound(page).isChecked(), true)
  assert.equal(await bound(page).isDisabled(), true)
  assert.equal(await exclusive(page).isChecked(), true)
  assert.equal(await exclusive(page).isDisabled(), true)
  assert.equal(await save(page).isEnabled(), true)
  assert.deepEqual(await page.evaluate(() => window.writes), [])
  await save(page).click()
  await page.getByRole('dialog').waitFor({ state: 'hidden' })
  assert.deepEqual(await page.evaluate(() => window.writes), [{ method: 'PATCH', path: '/api/admin/proxies/7/access', body: { exclusive: true, allowed_account_ids: [10000] } }])
})

test('all bound pages are included and an existing saved allowlist is preserved', async t => {
  const page = await fixture(t, { count: 101, allowed: [42, 10000] })
  await page.getByText('102 selected', { exact: true }).waitFor()
  await save(page).click()
  await page.getByRole('dialog').waitFor({ state: 'hidden' })
  const actual = await page.evaluate(() => ({ ids: window.writes[0].body.allowed_account_ids, pages: window.boundRequests.map(r => r.page) }))
  assert.deepEqual(actual.pages, [1, 2])
  assert.equal(actual.ids.length, 102)
  assert.equal(actual.ids.includes(42) && actual.ids.includes(10100), true)
})

test('selecting an account sets exclusive without a separate toggle; choices survive search and pagination', async t => {
  const page = await fixture(t, { count: 0 })
  await other(page).check()
  assert.equal(await exclusive(page).isChecked(), true)
  await page.getByRole('button', { name: 'Next', exact: true }).click()
  await other(page, 220).waitFor(); await other(page, 220).check()
  await page.getByRole('textbox', { name: 'Search accounts' }).fill('fixture')
  await other(page).waitFor()
  assert.equal(await other(page).isChecked(), true)
  assert.equal(await page.evaluate(() => window.boundRequests.length), 1)
  await save(page).click()
  await page.getByRole('dialog').waitFor({ state: 'hidden' })
  assert.deepEqual(await page.evaluate(() => window.writes[0].body.allowed_account_ids.sort((a, b) => a - b)), [200, 220])
})

test('an empty exclusive proxy can be reserved, and existing access remains intact', async t => {
  const empty = await fixture(t, { count: 0 })
  await exclusive(empty).check()
  await save(empty).click()
  await empty.getByRole('dialog').waitFor({ state: 'hidden' })
  assert.deepEqual(await empty.evaluate(() => window.writes[0].body), { exclusive: true, allowed_account_ids: [] })
  const existing = await fixture(t, { count: 0, exclusive: true, allowed: [200] })
  await other(existing).waitFor()
  assert.equal(await other(existing).isChecked(), true)
  await save(existing).click()
  await existing.getByRole('dialog').waitFor({ state: 'hidden' })
  assert.deepEqual(await existing.evaluate(() => window.writes[0].body.allowed_account_ids), [200])
})

test('loading blocks save and edits; partial failure applies nothing and can be retried', async t => {
  const page = await fixture(t, { count: 101, allowed: [42], hold: true, failPage: 2 })
  await page.getByRole('status').waitFor()
  assert.equal(await save(page).isDisabled(), true)
  assert.equal(await other(page).isDisabled(), true)
  await page.waitForFunction(() => Boolean(window.releaseBound))
  await page.evaluate(() => { window.holdBound = false; window.releaseBound() })
  await page.getByRole('alert').waitFor()
  assert.equal(await save(page).isDisabled(), true)
  assert.equal(await bound(page).count(), 0)
  assert.equal(await page.getByText('1 selected', { exact: true }).count(), 1)
  assert.deepEqual(await page.evaluate(() => window.writes), [])
  await page.evaluate(() => { window.failPage = 0 })
  await page.getByRole('button', { name: 'Retry loading bindings', exact: true }).click()
  await page.getByText('102 selected', { exact: true }).waitFor()
  assert.equal(await save(page).isEnabled(), true)
})

test('incomplete binding pagination fails closed rather than saving a partial selection', async t => {
  const page = await fixture(t, { count: 101, incomplete: true })
  await page.getByRole('alert').waitFor()
  assert.equal(await save(page).isDisabled(), true)
  assert.equal(await bound(page).count(), 0)
  assert.deepEqual(await page.evaluate(() => window.writes), [])
})

test('closing or switching proxies discards late binding responses and resets the editor', async t => {
  const page = await fixture(t, { hold: true })
  await page.waitForFunction(() => Boolean(window.releaseBound))
  await page.getByRole('button', { name: 'Cancel', exact: true }).click()
  await page.getByRole('dialog').waitFor({ state: 'hidden' })
  await page.evaluate(() => { window.setFixtureProxy({ id: 8, url: 'http://proxy.test:20008', exclusive: false, allowed_account_ids: [] }); window.holdBound = false; window.releaseBound() })
  await page.getByText('2 selected', { exact: true }).waitFor()
  await save(page).click()
  await page.getByRole('dialog').waitFor({ state: 'hidden' })
  const writes = await page.evaluate(() => window.writes)
  assert.equal(writes.length, 1)
  assert.equal(writes[0].path, '/api/admin/proxies/8/access')
  assert.deepEqual(writes[0].body.allowed_account_ids, [10000, 10001])
})
