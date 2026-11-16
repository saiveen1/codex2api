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
    name: 'account-proxy-harness', enforce: 'pre',
    resolveId(id) { if (id === 'virtual:account-proxy') return '\0account-proxy' },
    load(id) {
      if (id !== '\0account-proxy') return
      return `import React from 'react'; import {createRoot} from 'react-dom/client';
        import Editor from ${JSON.stringify(path.join(root, 'src/components/AccountProxyQuickEditor.tsx'))};
        import Badge from ${JSON.stringify(path.join(root, 'src/components/AccountProxyBadge.tsx'))};
        import {useProxyPool} from ${JSON.stringify(path.join(root, 'src/hooks/useProxyPool.ts'))};
        import {buildProxyBindingContext} from ${JSON.stringify(path.join(root, 'src/lib/accountProxyBinding.ts'))};
        import {ToastProvider} from ${JSON.stringify(path.join(root, 'src/components/ToastProvider.tsx'))};
        import ${JSON.stringify(path.join(root, 'src/i18n.ts'))};
        import ${JSON.stringify(path.join(root, 'src/index.css'))};
        function App(){
          const proxies=useProxyPool(); const [account,setAccount]=React.useState(null);
          const [accounts,setAccounts]=React.useState({...window.mockAccounts});
          const ctx=buildProxyBindingContext({proxies,poolEnabled:false});
          return React.createElement(React.Fragment,null,
            React.createElement('output',{'data-testid':'counts'},proxies.map(p=>p.id+':'+p.bound_count).join(',')),
            [11,12].map(id=>React.createElement('button',{key:id,onClick:()=>setAccount({...window.mockAccounts[id]})},'Edit '+id)),
            React.createElement('div',{'data-testid':'badge'},React.createElement(Badge,{account:accounts[11],ctx,onClick:()=>setAccount({...window.mockAccounts[11]})})),
            React.createElement(Editor,{account,accountLabel:'Synthetic account',proxies,ctx,onClose:()=>setAccount(null),onSaved:async()=>{setAccounts({...window.mockAccounts});if(window.failReload)throw new Error('synthetic reload failure')}}));
        }
        createRoot(document.getElementById('root')).render(React.createElement(ToastProvider,null,React.createElement(App)));`
    },
  }], resolve: { alias: { '@': path.join(root, 'src') } }, define: { 'process.env.NODE_ENV': '"production"' },
    build: { write: false, minify: false, rollupOptions: { input: 'virtual:account-proxy' }, lib: { entry: 'virtual:account-proxy', formats: ['iife'], name: 'AccountProxyHarness' } },
  })
  const bundle = Array.isArray(result) ? result[0] : result
  script = bundle.output.find(item => item.type === 'chunk').code
  css = bundle.output.filter(item => item.type === 'asset' && item.fileName.endsWith('.css')).map(item => item.source).join('\n')
  browser = await chromium.launch({ headless: true, executablePath: process.env.CHROMIUM_PATH || undefined, args: ['--no-sandbox'] })
})
after(async () => { await browser?.close() })

async function fixture(t, mode = '') {
  const page = await browser.newPage({ viewport: { width: 1280, height: 960 } })
  page.setDefaultTimeout(5_000)
  const errors = []
  page.on('pageerror', error => errors.push(error.message))
  t.after(() => assert.deepEqual(errors, []))
  t.after(() => page.close())
  await page.route('http://account-proxy.test/**', route => route.fulfill({ contentType: 'text/html', body: '<div id="root"></div>' }))
  await page.goto('http://account-proxy.test/')
  await page.evaluate(mode => {
    localStorage.setItem('lang', 'en')
    const url = id => `http://synthetic-user:private-password@127.0.0.1:${21000 + id}`
    window.mockAccounts = { 11: { id: 11, proxy_url: mode === 'disabled' ? url(2) : '' }, 12: { id: 12, proxy_url: '' } }
    window.pool = [
      { id: 1, label: 'Primary subscription / direct US01 - full untruncated node name', enabled: true },
      { id: 2, label: 'Excluded metadata node', enabled: false },
      { id: 3, label: 'Failed node', enabled: true, test_status: 'error' },
      { id: 4, label: 'Owned exclusive node', enabled: true, exclusive: true, allowed_account_ids: [11] },
      { id: 5, label: 'Foreign exclusive node', enabled: true, exclusive: true, allowed_account_ids: [99] },
    ].map(p => ({ test_status: 'untested', ...p, enabled: mode === 'empty' ? false : p.enabled, url: url(p.id) }))
    window.reads = 0
    window.fetch = async (url, options = {}) => {
      const pathname = new URL(String(url), location.origin).pathname
      if (pathname.endsWith('/proxies')) {
        window.reads++
        const snapshot = window.pool.map(p => ({ ...p, bound_count: Object.values(window.mockAccounts).filter(a => a.proxy_url === p.url).length }))
        if (window.deferRead) {
          window.deferRead = false
          return new Promise(resolve => { window.releaseRead = () => resolve(new Response(JSON.stringify({ proxies: snapshot }))) })
        }
        return new Response(JSON.stringify({ proxies: snapshot }))
      }
      const match = pathname.match(/\/accounts\/(\d+)\/scheduler$/)
      if (match) {
        if (window.failSave) return new Response(JSON.stringify({ error: 'synthetic save failure' }), { status: 400 })
        window.mockAccounts[match[1]] = { ...window.mockAccounts[match[1]], ...JSON.parse(options.body) }
        return new Response(JSON.stringify({ success: true }))
      }
      throw new Error('Unexpected fixture request')
    }
  }, mode)
  await page.addStyleTag({ content: css })
  await page.addScriptTag({ content: script })
  await page.waitForFunction(() => document.querySelector('[data-testid=counts]')?.textContent.includes('1:0'))
  return page
}

async function open(t, page, id) {
  await page.getByRole('button', { name: `Edit ${id}`, exact: true }).click()
  const dialog = page.getByRole('dialog')
  await dialog.waitFor()
  const trigger = dialog.locator('button[aria-haspopup=listbox]')
  await trigger.click()
  return { dialog, trigger, list: page.getByRole('listbox') }
}

async function save(page, dialog) {
  await dialog.getByRole('button', { name: 'Save', exact: true }).click()
  await dialog.waitFor({ state: 'hidden' })
}

test('filters excluded nodes, shows full masked identity and hot-refreshes bind/rebind/unbind', async t => {
  const page = await fixture(t)
  let { dialog, trigger, list } = await open(t, page, 11)
  assert.equal(await list.getByRole('option').count(), 2)
  assert.equal(await list.getByText('Excluded metadata node').count(), 0)
  assert.doesNotMatch(await list.innerText(), /private-password|synthetic-user/)
  await list.getByRole('option', { name: /#1 ·/ }).click()
  assert.match(await trigger.innerText(), /#1/)
  await save(page, dialog)
  await page.waitForFunction(() => document.querySelector('[data-testid=counts]').textContent.startsWith('1:1'))
  const tip = await page.locator('[data-testid=badge] button').getAttribute('title')
  assert.match(tip, /#1 · Primary subscription \/ direct US01 - full untruncated node name/)
  assert.match(tip, /1 accounts/)
  assert.doesNotMatch(tip, /private-password|synthetic-user/)
  ;({ dialog, list } = await open(t, page, 12))
  assert.equal(await list.getByRole('option').count(), 1)
  assert.match(await list.getByRole('option').innerText(), /1 accounts/)
  await list.getByRole('option').click()
  await save(page, dialog)
  await page.waitForFunction(() => document.querySelector('[data-testid=counts]').textContent.startsWith('1:2'))
  ;({ dialog, list } = await open(t, page, 11))
  await list.getByRole('option', { name: /#4 ·/ }).click()
  await save(page, dialog)
  await page.waitForFunction(() => document.querySelector('[data-testid=counts]').textContent.includes('4:1'))
  assert.match(await page.locator('[data-testid=counts]').innerText(), /^1:1/)
  await page.getByRole('button', { name: 'Edit 11', exact: true }).click()
  await page.getByRole('dialog').getByRole('button', { name: /Unbind/ }).click()
  await page.getByRole('dialog').waitFor({ state: 'hidden' })
  await page.waitForFunction(() => document.querySelector('[data-testid=counts]').textContent.includes('4:0'))
})

test('saved disabled selection remains identifiable but cannot be chosen again', async t => {
  const page = await fixture(t, 'disabled')
  const { trigger, list } = await open(t, page, 11)
  assert.match(await trigger.innerText(), /#2.*Excluded metadata node.*Unavailable/s)
  assert.equal(await list.getByRole('option', { name: /#2 ·/ }).count(), 0)
})

test('empty candidate pool is explicit and preserves manual entry', async t => {
  const page = await fixture(t, 'empty')
  const { dialog, list } = await open(t, page, 11)
  assert.equal(await list.getByRole('option').count(), 0)
  assert.match(await list.innerText(), /No eligible proxies/)
  await page.keyboard.press('Escape')
  await dialog.locator('input').fill('http://custom.example:8080')
  await save(page, dialog)
  assert.equal(await page.evaluate(() => window.mockAccounts[11].proxy_url), 'http://custom.example:8080')
})

test('failed saves do not change counts; delayed reads cannot overwrite fresh occupancy', async t => {
  const page = await fixture(t)
  let { dialog, list } = await open(t, page, 11)
  await list.getByRole('option', { name: /#1 ·/ }).click()
  await page.evaluate(() => { window.failSave = true })
  await dialog.getByRole('button', { name: 'Save', exact: true }).click()
  await page.getByText(/synthetic save failure/).waitFor()
  assert.match(await page.locator('[data-testid=counts]').innerText(), /^1:0/)
  await page.evaluate(() => { window.failSave = false; window.deferRead = true; window.dispatchEvent(new Event('focus')) })
  await page.waitForFunction(() => Boolean(window.releaseRead))
  await save(page, dialog)
  await page.waitForFunction(() => document.querySelector('[data-testid=counts]').textContent.startsWith('1:1'))
  await page.evaluate(async () => { window.releaseRead(); await new Promise(resolve => requestAnimationFrame(() => requestAnimationFrame(resolve))) })
  assert.match(await page.locator('[data-testid=counts]').innerText(), /^1:1/)
})

test('account reload failure after a successful save is not reported as a failed binding', async t => {
  const page = await fixture(t)
  const { dialog, list } = await open(t, page, 11)
  await list.getByRole('option', { name: /#1 ·/ }).click()
  await page.evaluate(() => { window.failReload = true })
  await save(page, dialog)
  await page.getByText(/Proxy binding was saved/).waitFor()
  assert.equal(await page.getByText(/Failed to update account proxy/).count(), 0)
  await page.waitForFunction(() => document.querySelector('[data-testid=counts]').textContent.startsWith('1:1'))
})
