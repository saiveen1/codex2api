import assert from 'node:assert/strict'
import { after, before, test } from 'node:test'
import path from 'node:path'
import { fileURLToPath } from 'node:url'
import { readFile } from 'node:fs/promises'
import { build } from 'vite'
import react from '@vitejs/plugin-react'
import tailwindcss from '@tailwindcss/vite'
import { chromium } from 'playwright'

const root = fileURLToPath(new URL('../', import.meta.url))
let browser, script, css
before(async () => {
  const accountsSource = await readFile(path.join(root, 'src/pages/Accounts.tsx'), 'utf8')
  const statusStart = accountsSource.indexOf('data-account-state-cell="status"')
  const statusSource = accountsSource.slice(statusStart, accountsSource.indexOf('<AccountHealthBar', statusStart))
  const outerClass = statusSource.match(/className="([^"]*max-w-\[240px\][^"]*)"/)[1]
  const webClass = statusSource.match(/className="([^"]*)" data-account-web-row/)[1]
  // Keep this isolated layout harness tied to the actual table row, without mounting the entire admin page.
  assert.match(statusSource, /<\/div>\s*\{isOAuthAccount\(account\) && <div[^>]*data-account-web-row><ChatGPTWebAccountBadge/)
  assert.doesNotMatch(statusSource.slice(0, statusSource.indexOf('data-account-web-row')), /shrink-0[^]*?<ChatGPTWebAccountBadge/)
  const result = await build({ configFile: false, root, logLevel: 'silent', plugins: [react(), tailwindcss(), {
    name: 'web-batch-layout-harness', enforce: 'pre',
    resolveId(id) { if (id === 'virtual:web-batch-layout') return '\0web-batch-layout' },
    load(id) {
      if (id !== '\0web-batch-layout') return
      return `import React from 'react'; import {createRoot} from 'react-dom/client';
        import Settings from ${JSON.stringify(path.join(root, 'src/components/ChatGPTWebSettings.tsx'))};
        import Badge from ${JSON.stringify(path.join(root, 'src/components/ChatGPTWebAccountBadge.tsx'))};
        import {MemoryRouter} from 'react-router-dom';
        import ${JSON.stringify(path.join(root, 'src/i18n.ts'))}; import ${JSON.stringify(path.join(root, 'src/index.css'))};
        const h=React.createElement;
        function Layout(){return h('table',{className:'w-full table-fixed'},h('tbody',null,h('tr',null,
          h('td',{'data-testid':'status-cell',className:'align-top',style:{width:innerWidth<500?'180px':'240px'}},
            h('div',{className:${JSON.stringify(outerClass)}},
              h('div',{'data-testid':'native-row',className:'flex min-h-6 flex-nowrap items-center gap-1.5 whitespace-nowrap'},h('span',null,'Healthy'),h('span',null,'2/5')),
              h('div',{'data-account-web-row':true,className:${JSON.stringify(webClass)}},h(Badge,{accountID:7,accountName:'layout-fixture',state:{account_id:7,enabled:true,status:'unverified',image_quota_remaining:123456,capabilities:[]}})))),
          h('td',{'data-testid':'next-cell',className:'align-top'},'Next column'))))}
        createRoot(document.getElementById('root')).render(h(MemoryRouter,null,window.__layout?h(Layout):h(Settings)));`
    },
  }], resolve: { alias: { '@': path.join(root, 'src') } }, define: { 'process.env.NODE_ENV': '"production"' },
    build: { write: false, minify: false, rollupOptions: { input: 'virtual:web-batch-layout' }, lib: { entry: 'virtual:web-batch-layout', formats: ['iife'], name: 'WebBatchLayoutHarness' } },
  })
  const bundle = Array.isArray(result) ? result[0] : result
  script = bundle.output.find(item => item.type === 'chunk').code
  css = bundle.output.filter(item => item.type === 'asset' && item.fileName.endsWith('.css')).map(item => item.source).join('\n')
  browser = await chromium.launch({ headless: true, executablePath: process.env.CHROMIUM_PATH || undefined, args: ['--no-sandbox'] })
})
after(async () => { await browser?.close() })

async function fixture(t, { layout = false, width = 1000, fail = false } = {}) {
  const page = await browser.newPage({ viewport: { width, height: 900 } })
  page.setDefaultTimeout(5000)
  const errors = []
  page.on('pageerror', error => errors.push(error.message))
  t.after(() => assert.deepEqual(errors, []))
  t.after(() => page.close())
  await page.route('http://web-batch.test/**', route => route.fulfill({ contentType: 'text/html', body: '<main id="root" class="p-4 min-w-0"></main>' }))
  await page.goto('http://web-batch.test/')
  await page.evaluate(({ layout, fail }) => {
    localStorage.setItem('lang', 'en')
    window.__layout = layout
    window.__writes = []
    const states = [7, 8, 9].map(account_id => ({ account_id, enabled: false, status: 'unverified', capabilities: [], image_quota_remaining: 12, observed_at: '2026-11-16T08:00:00Z' }))
    window.fetch = async (url, init = {}) => {
      const pathname = new URL(String(url), location.origin).pathname
      const method = init.method || 'GET'
      let body, status = 200
      if (method !== 'GET') window.__writes.push({ pathname, method, body: JSON.parse(init.body || '{}') })
      if (method === 'GET' && pathname === '/api/admin/chatgpt-web') body = { enabled: true, configured: true, protocol_version: 'fixture', supported_models: ['gpt-image-2'] }
      else if (method === 'GET' && pathname === '/api/admin/accounts') body = { accounts: [7, 8, 9].map(id => ({ id, name: 'OAuth ' + id, account_type: 'oauth' })).concat([{ id: 99, name: 'Relay fixture', account_type: 'openai_responses_api', openai_responses_api: {} }]) }
      else if (method === 'GET' && pathname === '/api/admin/account-groups') body = { groups: [{ id: 11, name: 'Current group', channel: 'codex' }, { id: 12, name: 'Other channel', channel: 'claude' }] }
      else if (method === 'GET' && pathname === '/api/admin/keys') body = { keys: [{ id: 21, name: 'Web key', key: 'SECRET_MASKED', raw_key: 'SECRET_RAW', limits: { upstream_channel: 'chatgpt_web' } }, { id: 22, name: 'Native key', limits: { upstream_channel: 'codex' } }] }
      else if (method === 'GET' && pathname === '/api/admin/chatgpt-web/accounts') body = { accounts: states }
      else if (method === 'GET' && pathname === '/api/admin/chatgpt-web/executions') body = { executions: [] }
      else if (method === 'POST' && pathname === '/api/admin/chatgpt-web/accounts/batch') {
        const request = JSON.parse(init.body)
        if (fail) { status = 400; body = { error: 'Synthetic scope failure', code: 'account_ineligible', stage: 'admission' } }
        else {
          const ids = request.scope === 'accounts' ? request.account_ids : request.scope === 'group' ? [7, 8] : [8, 9]
          const results = ids.map((id, index) => {
            if (index === 1) return { account_id: id, changed: false, code: 'account_busy', error: 'Synthetic busy account' }
            const state = states.find(state => state.account_id === id)
            const changed = state.enabled !== request.enabled
            state.enabled = request.enabled
            return { account_id: id, state, changed }
          })
          const updated = results.filter(item => item.changed).length
          body = { matched: ids.length, updated, unchanged: ids.length - updated - 1, failed: 1, results }
        }
      } else throw new Error('Unexpected request: ' + method + ' ' + pathname)
      return new Response(JSON.stringify(body), { status, headers: { 'Content-Type': 'application/json' } })
    }
  }, { layout, fail })
  await page.addStyleTag({ content: css })
  await page.addScriptTag({ content: script })
  if (!layout) await page.waitForFunction(() => document.querySelector('section[aria-label="Batch Web account control"]')?.getAttribute('aria-busy') === 'false' && document.querySelectorAll('input[type=checkbox]').length === 3)
  return page
}

async function batchDone(page) {
  await page.getByTestId('web-batch-summary').waitFor()
  await page.waitForFunction(() => document.querySelector('section[aria-label="Batch Web account control"]')?.getAttribute('aria-busy') === 'false')
}

test('multi-account batch uses only explicit IDs and shows truthful partial results with synced badges', async t => {
  const page = await fixture(t)
  assert.deepEqual(await page.evaluate(() => window.__writes), [])
  assert.equal(await page.getByRole('checkbox').count(), 3)
  assert.equal(await page.getByRole('button', { name: 'Enable selected scope', exact: true }).isDisabled(), true)
  await page.getByRole('checkbox', { name: '#7 OAuth 7', exact: true }).check()
  await page.getByRole('checkbox', { name: '#8 OAuth 8', exact: true }).check()
  await page.getByRole('button', { name: 'Enable selected scope', exact: true }).click()
  await batchDone(page)
  assert.match(await page.getByTestId('web-batch-summary').textContent(), /Matched 2 · Updated 1 · Unchanged 0 · Failed 1.*#8 · account_busy/)
  assert.deepEqual(await page.evaluate(() => window.__writes), [{ pathname: '/api/admin/chatgpt-web/accounts/batch', method: 'POST', body: { scope: 'accounts', enabled: true, account_ids: [7, 8] } }])
  assert.match(await page.locator('[data-web-account="7"] button').first().textContent(), /Check required/)
  assert.match(await page.locator('[data-web-account="8"] button').first().textContent(), /Disabled/)
  await page.getByRole('button', { name: 'Enable selected scope', exact: true }).click()
  await batchDone(page)
  assert.match(await page.getByTestId('web-batch-summary').textContent(), /Matched 2 · Updated 0 · Unchanged 1 · Failed 1/)
})

test('group and Web-key scopes send server selectors, never expanded IDs or secret key strings', async t => {
  const page = await fixture(t, { width: 390 })
  await page.getByLabel('Target scope', { exact: true }).selectOption('group')
  assert.equal(await page.getByRole('button', { name: 'Enable selected scope', exact: true }).isDisabled(), true)
  await page.getByLabel('Account group', { exact: true }).selectOption('11')
  assert.doesNotMatch(await page.getByLabel('Account group', { exact: true }).textContent(), /Other channel/)
  await page.getByRole('button', { name: 'Enable selected scope', exact: true }).click()
  await batchDone(page)
  await page.getByLabel('Target scope', { exact: true }).selectOption('api_key')
  const keys = page.getByLabel('Web API key', { exact: true })
  assert.doesNotMatch(await keys.textContent(), /Native key|SECRET/)
  assert.doesNotMatch(await page.content(), /SECRET_RAW|SECRET_MASKED/)
  await keys.selectOption('21')
  assert.deepEqual(await page.evaluate(() => window.__writes.map(item => item.body)), [{ scope: 'group', enabled: true, group_id: 11 }], 'selecting a target must not mutate it')
  await page.getByRole('button', { name: 'Disable selected scope', exact: true }).click()
  await batchDone(page)
  assert.deepEqual(await page.evaluate(() => window.__writes.map(item => item.body)), [{ scope: 'group', enabled: true, group_id: 11 }, { scope: 'api_key', enabled: false, api_key_id: 21 }])
  assert.equal(await page.evaluate(() => document.documentElement.scrollWidth <= innerWidth), true)
})

test('batch scope errors remain visible after snapshot reconciliation', async t => {
  const page = await fixture(t, { fail: true })
  await page.getByRole('checkbox', { name: '#7 OAuth 7', exact: true }).check()
  await page.getByRole('button', { name: 'Enable selected scope', exact: true }).click()
  await page.getByRole('alert').waitFor()
  await page.waitForFunction(() => document.querySelector('section[aria-label="Batch Web account control"]')?.getAttribute('aria-busy') === 'false')
  assert.match(await page.getByRole('alert').textContent(), /Synthetic scope failure.*code: account_ineligible.*stage: admission/)
  assert.equal(await page.getByTestId('web-batch-summary').count(), 0)
  assert.match(await page.locator('[data-web-account="7"] button').first().textContent(), /Disabled/)
})

test('Web badge occupies its own bounded row without overlapping native status or the next column', async t => {
  for (const width of [1280, 390]) {
    const page = await fixture(t, { layout: true, width })
    const badge = page.getByRole('button', { name: 'View Web image quota for layout-fixture', exact: true })
    await badge.waitFor()
    const bounds = await badge.boundingBox()
    const native = await page.getByTestId('native-row').boundingBox()
    const cell = await page.getByTestId('status-cell').boundingBox()
    const next = await page.getByTestId('next-cell').boundingBox()
    assert.equal(bounds.y >= native.y + native.height, true, `own row at ${width}px`)
    assert.equal(bounds.x >= cell.x && bounds.x + bounds.width <= cell.x + cell.width + 1, true, `bounded badge at ${width}px`)
    assert.equal(bounds.x + bounds.width <= next.x + 1, true, `no adjacent-column overlap at ${width}px`)
    assert.equal(await page.evaluate(() => document.documentElement.scrollWidth <= innerWidth), true)
    assert.deepEqual(await page.evaluate(() => window.__writes), [])
  }
})
