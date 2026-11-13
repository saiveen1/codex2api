import assert from 'node:assert/strict'
import { after, before, test } from 'node:test'
import { fileURLToPath } from 'node:url'
import { mkdir, mkdtemp } from 'node:fs/promises'
import { tmpdir } from 'node:os'
import path from 'node:path'
import { build } from 'vite'
import react from '@vitejs/plugin-react'
import tailwindcss from '@tailwindcss/vite'
import { chromium } from 'playwright'

const root = fileURLToPath(new URL('../', import.meta.url))
const origin = 'http://127.0.0.1:41783'
const pendingKey = 'codex2api:oauth-gateway:pending-request'
let browser, script, css, artifacts
before(async () => {
  artifacts = process.env.OAUTH_GATEWAY_TEST_ARTIFACT_DIR || await mkdtemp(path.join(tmpdir(), 'codex2api-oauth-'))
  await mkdir(artifacts, { recursive: true })
  const result = await build({ configFile: false, root, logLevel: 'error', plugins: [react(), tailwindcss(), {
    name: 'oauth-fixture', enforce: 'pre',
    resolveId(id) { if (id === 'virtual:oauth-fixture') return '\0oauth-fixture'; if (/^(?:\.\.\/|\.\/)+api$/.test(id)) return '\0oauth-admin-auth' },
    load(id) {
      if (id === '\0oauth-admin-auth') return 'export const getAdminKey=()=>"synthetic-admin"; export const resetAdminAuthState=()=>{window.__loggedOut=true}'
      if (id !== '\0oauth-fixture') return
      return `import React from 'react'; import {createRoot} from 'react-dom/client'; import {MemoryRouter} from 'react-router-dom';
        import OAuthGateway from ${JSON.stringify(path.join(root, 'src/pages/OAuthGateway.tsx'))};
        import ${JSON.stringify(path.join(root, 'src/i18n.ts'))}; import ${JSON.stringify(path.join(root, 'src/index.css'))};
        const root=createRoot(document.getElementById('root')); let generation=0;
        window.__remount=()=>root.render(React.createElement(MemoryRouter,null,React.createElement('main',{style:{padding:'20px',maxWidth:'1280px',margin:'auto'}},React.createElement(OAuthGateway,{key:++generation})))); window.__remount();`
    },
  }], resolve: { alias: { '@': path.join(root, 'src') } }, define: { 'process.env.NODE_ENV': '"production"' }, build: { write: false, minify: false, rollupOptions: { input: 'virtual:oauth-fixture' }, lib: { entry: 'virtual:oauth-fixture', formats: ['iife'], name: 'OAuthFixture' } } })
  const output = (Array.isArray(result) ? result[0] : result).output
  script = output.find(item => item.type === 'chunk').code
  css = output.filter(item => item.type === 'asset' && item.fileName.endsWith('.css')).map(item => item.source).join('\n')
  browser = await chromium.launch({ headless: true })
})
after(async () => { await browser?.close() })

async function fixture(t, { mobile = false, uncertain = false } = {}) {
  const page = await browser.newPage({ viewport: mobile ? { width: 390, height: 844 } : { width: 1280, height: 1100 }, reducedMotion: 'reduce' })
  page.setDefaultTimeout(7000)
  t.after(() => page.close())
  const errors = []
  page.on('pageerror', error => errors.push(error.message))
  t.after(() => assert.deepEqual(errors, []))
  const state = { submissions: 0, lookups: [], job: null, archived: [], lastInput: null }
  await page.route('**/*', async route => {
    const request = route.request(), url = new URL(request.url())
    if (url.origin !== origin) throw new Error('unexpected external network in OAuth UI test')
    if (!url.pathname.startsWith('/api/admin/oauth-gateway/')) return route.fulfill({ contentType: 'text/html', body: '<!doctype html><html><head><meta name="viewport" content="width=device-width,initial-scale=1"></head><body><div id="root"></div></body></html>' })
    assert.equal(request.headers()['x-admin-key'], 'synthetic-admin')
    const p = url.pathname.slice('/api/admin/oauth-gateway'.length)
    const send = (data, status = 200) => route.fulfill({ status, contentType: 'application/json', body: JSON.stringify({ ok: true, data }) })
    if (p === '/settings') return send({ oauth_url: 'https://oauth.cala.lol', oauth_token_set: true, configured: true, auto_recovery_supported: false, auto_recovery_reason: '' })
    if (p === '/capabilities') return send({ countries: ['auto', 'US'], environments: ['auto'], proxy_modes: ['system', 'custom'], workspace_policies: ['team_preferred'], max_batch_size: 100, max_upload_bytes: 1048576, features: { proxy_check: true } })
    if (p === '/quota') return send({ total: 10, remaining: 9, reserved: 0, used: 1 })
    if (p === '/groups') return send([{ id: 1, name: '测试分组' }])
    if (p === '/preview') return send({ valid: true, count: 1, groups: 1, rows: [{ line: 1, identifier_masked: 'f***@example.invalid', proxy_mode: 'system' }], errors: [] })
    if (p === '/jobs' && request.method() === 'POST') {
      state.submissions++
      state.lastInput = request.postDataJSON()
      state.job = { id: state.lastInput.request_id, state: 'running', total: 1, imported: 0, failed: 0, pending: 1, can_refresh: true, can_cancel: true, can_retry_import: false, can_archive: false, retained_material: false, archived: false, groups: [{ index: 0, state: 'running', batch_id: 'gb_fixture', request_id: 'bridge_fixture', imported: 0, total: 1, items: [{ line: 1, identifier_masked: 'f***@example.invalid', state: 'running', import_state: 'pending' }] }] }
      if (uncertain) return route.fulfill({ status: 503, contentType: 'application/json', body: JSON.stringify({ ok: false, error: { code: 'synthetic_response_lost', message: 'Synthetic uncertain response' } }) })
      return send(state.job, 202)
    }
    if (p === '/jobs') return send(url.searchParams.has('archived') ? state.archived : state.job && !state.job.archived ? [state.job] : [])
    if (p === `/jobs/${state.job?.id}/archive`) { state.job = { ...state.job, archived: true }; state.archived.push(state.job); return send(state.job) }
    if (p.startsWith('/jobs/') && request.method() === 'GET') { state.lookups.push(p.slice('/jobs/'.length)); return send(state.job) }
    throw new Error(`unhandled fixture path: ${request.method()} ${p}`)
  })
  await page.goto(origin)
  await page.evaluate(() => localStorage.setItem('lang', 'zh'))
  await page.addStyleTag({ content: css })
  await page.addScriptTag({ content: script })
  await page.getByRole('heading', { name: 'OAuth 网关', exact: true }).waitFor()
  return { page, state }
}
async function prepare(page) {
  await page.getByRole('button', { name: '读取能力和配额', exact: true }).click()
  await page.getByText(/配额剩余 \/ 总计/).waitFor()
  await page.getByLabel('账号材料', { exact: true }).fill('fixture@example.invalid----  synthetic-password  ----JBSWY3DPEHPK3PXP')
  await page.getByRole('button', { name: '预检', exact: true }).click()
  await page.getByText(/预检通过/).waitFor()
}
test('preview gates submission, live details retain expansion, archive uses existing job', async t => {
  const { page, state } = await fixture(t)
  const submit = page.getByRole('button', { name: '创建授权任务', exact: true })
  assert.equal(await submit.isDisabled(), true)
  await prepare(page)
  await page.getByLabel('时区（可选）').fill('UTC')
  assert.equal(await submit.isDisabled(), true)
  await page.getByRole('button', { name: '预检', exact: true }).click()
  await page.getByText(/预检通过/).waitFor()
  await submit.click()
  await page.getByText(/任务已接受：/).waitFor()
  assert.equal(state.submissions, 1)
  assert.ok(state.lastInput.source_text.includes('----  synthetic-password  ----'))
  assert.equal(await page.getByLabel('账号材料', { exact: true }).inputValue(), '')
  assert.equal(await page.evaluate(key => sessionStorage.getItem(key), pendingKey), null)
  await page.getByText('批次与账号详情', { exact: true }).click()
  assert.equal(await page.getByRole('button', { name: '归档', exact: true }).isDisabled(), true)
  state.job = { ...state.job, state: 'completed', imported: 1, pending: 0, can_refresh: false, can_cancel: false, can_archive: true, groups: [{ ...state.job.groups[0], state: 'complete', imported: 1, items: [{ line: 1, identifier_masked: 'f***@example.invalid', state: 'succeeded', import_state: 'imported' }] }] }
  await page.locator('details[open]').getByText(/imported/).waitFor()
  await page.screenshot({ path: path.join(artifacts, 'oauth-desktop.png'), fullPage: true })
  page.once('dialog', dialog => dialog.accept())
  await page.getByRole('button', { name: '归档', exact: true }).click()
  await page.getByRole('button', { name: '归档任务', exact: true }).click()
  await page.getByText(/已归档/).waitFor()
  assert.equal(state.submissions, 1)
  assert.equal(await page.evaluate(() => document.documentElement.scrollWidth <= innerWidth), true)
})
test('uncertain request survives in-app navigation without storing material or resubmitting', async t => {
  const { page, state } = await fixture(t, { uncertain: true })
  await prepare(page)
  await page.getByRole('button', { name: '创建授权任务', exact: true }).click()
  await page.getByText(/结果不确定的请求 ID/).waitFor()
  const originalID = state.job.id
  assert.equal(await page.evaluate(key => sessionStorage.getItem(key), pendingKey), originalID)
  assert.equal(await page.getByLabel('账号材料', { exact: true }).inputValue(), '')
  await page.evaluate(() => window.__remount())
  await page.getByText(/结果不确定的请求 ID/).waitFor()
  assert.equal(await page.getByRole('button', { name: '创建授权任务', exact: true }).isDisabled(), true)
  const stored = await page.evaluate(() => JSON.stringify({ ...sessionStorage, ...localStorage }))
  for (const secret of ['synthetic-password', 'JBSWY3DPEHPK3PXP', 'fixture@example.invalid', 'synthetic-admin']) assert.equal(stored.includes(secret), false)
  await page.getByRole('button', { name: '查找原任务', exact: true }).click()
  await page.getByText(/已找到原任务：/).waitFor()
  assert.deepEqual(state.lookups, [originalID])
  assert.equal(state.submissions, 1)
  assert.equal(await page.evaluate(key => sessionStorage.getItem(key), pendingKey), null)
})
test('mobile layout fits and exposes keyboard-operable controls', async t => {
  const { page } = await fixture(t, { mobile: true })
  await page.getByRole('button', { name: '读取能力和配额', exact: true }).focus()
  await page.keyboard.press('Enter')
  await page.getByText(/配额剩余 \/ 总计/).waitFor()
  assert.equal(await page.evaluate(() => document.documentElement.scrollWidth <= innerWidth), true)
  await page.screenshot({ path: path.join(artifacts, 'oauth-mobile.png'), fullPage: true })
})
