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
const material = 'fixture@example.invalid----  synthetic-password  ----JBSWY3DPEHPK3PXP'
let browser, script, css, artifacts
before(async () => {
  // Keep the JSX transform and the in-memory React bundle in the same mode.
  process.env.NODE_ENV = 'production'
  artifacts = process.env.OAUTH_GATEWAY_TEST_ARTIFACT_DIR || await mkdtemp(path.join(tmpdir(), 'codex2api-oauth-'))
  await mkdir(artifacts, { recursive: true })
  const result = await build({ configFile: false, root, logLevel: 'error', plugins: [react(), tailwindcss(), {
    name: 'oauth-fixture', enforce: 'pre',
    resolveId(id) { if (id === 'virtual:oauth-fixture') return '\0oauth-fixture'; if (/^(?:\.\.\/|\.\/)+api$/.test(id)) return '\0oauth-admin-auth' },
    load(id) {
      if (id === '\0oauth-admin-auth') return `export const getAdminKey=()=>"synthetic-admin"; export const resetAdminAuthState=()=>{window.__loggedOut=true}; export const PROXY_POOL_CHANGED="synthetic-proxy-change";
        const request=async(path,init)=>{const r=await fetch('/api/admin'+path,{...init,headers:{'X-Admin-Key':getAdminKey()}});if(!r.ok)throw new Error('Synthetic proxy request failed');return r.json()};
        export const api={listProxies:()=>request('/proxies'),testProxy:(url,id,lang,signal)=>request('/proxies/test',{method:'POST',body:JSON.stringify({url,id,lang}),signal})}`
      if (id !== '\0oauth-fixture') return
      return `import React from 'react'; import {createRoot} from 'react-dom/client'; import {MemoryRouter} from 'react-router-dom';
        import OAuthGateway from ${JSON.stringify(path.join(root, 'src/pages/OAuthGateway.tsx'))};
        import {gatewayRequest} from ${JSON.stringify(path.join(root, 'src/lib/oauthGatewayApi.ts'))};
        import ${JSON.stringify(path.join(root, 'src/i18n.ts'))}; import ${JSON.stringify(path.join(root, 'src/index.css'))};
        const root=createRoot(document.getElementById('root')); let generation=0; window.__gatewayRequest=gatewayRequest;
        window.__remount=()=>root.render(React.createElement(MemoryRouter,null,React.createElement('main',{style:{padding:'20px',maxWidth:'1280px',margin:'auto'}},React.createElement(OAuthGateway,{key:++generation})))); window.__remount();`
    },
  }], resolve: { alias: { '@': path.join(root, 'src') } }, define: { 'process.env.NODE_ENV': '"production"' }, build: { write: false, minify: false, rollupOptions: { input: 'virtual:oauth-fixture' }, lib: { entry: 'virtual:oauth-fixture', formats: ['iife'], name: 'OAuthFixture' } } })
  const output = (Array.isArray(result) ? result[0] : result).output
  script = output.find(item => item.type === 'chunk').code
  css = output.filter(item => item.type === 'asset' && item.fileName.endsWith('.css')).map(item => item.source).join('\n')
  browser = await chromium.launch({ headless: true, executablePath: process.env.CHROMIUM_PATH || undefined, args: ['--no-sandbox'] })
})
after(async () => { await browser?.close() })

async function fixture(t, options = {}) {
  const page = await browser.newPage({ viewport: options.mobile ? { width: 390, height: 844 } : { width: 1280, height: 1100 }, reducedMotion: 'reduce' })
  page.setDefaultTimeout(7000)
  t.after(() => page.close())
  const errors = []
  page.on('pageerror', error => errors.push(error.message))
  t.after(() => assert.deepEqual(errors, []))
  const state = {
    submissions: 0, lookups: [], requests: [], job: null, archived: [], lastInput: null, previewInput: null,
    jobs: options.jobs || [], archivedIDs: [],
    failures: { capabilities: options.capabilityFailures || 0, quota: options.quotaFailures || 0, groups: options.groupFailures || 0 },
    settings: { oauth_url: 'https://oauth.cala.lol', oauth_token_set: true, configured: true, auto_recovery_supported: true, auto_recovery_reason: '', max_jobs: 200, max_archived_jobs: 10000 },
    caps: { countries: ['auto', 'US'], environments: ['auto', 'custom_ua'], proxy_modes: ['system', 'custom'], workspace_policies: ['team_preferred'], max_batch_size: 100, max_upload_bytes: 1048576, features: { proxy_check: true, expected_identity_guard: true } },
    proxyTests: [],
    quota: { total: 10, remaining: 9, reserved: 0, used: 1 }, groups: options.emptyGroups ? [] : [{ id: 1, name: '测试分组' }],
    recoveries: options.recoveries || [], disabledRecoveries: [], missingLookup: false,
    proxies: options.proxies || [{ id: 1, url: 'http://one-runtime.example.invalid:8080', label: '运行节点一', enabled: true, test_status: 'success', bound_count: 0, exclusive: false }, { id: 2, url: 'http://two-runtime.example.invalid:8080', label: '运行节点二', enabled: true, test_status: 'untested', bound_count: 0, exclusive: false }],
  }
  await page.route('**/*', async route => {
    const request = route.request(), url = new URL(request.url())
    if (url.origin !== origin) throw new Error('unexpected external network in OAuth UI test')
    if (url.pathname === '/api/admin/proxies/test') {
      assert.equal(request.headers()['x-admin-key'], 'synthetic-admin')
      const payload = request.postDataJSON()
      state.proxyTests.push(payload)
      const result = payload.url === state.proxies[0].url ? { success: true, conclusive: true, latency_ms: 123 } : { success: false, conclusive: true }
      return route.fulfill({ status: 200, json: result })
    }
    if (url.pathname === '/api/admin/proxies') return route.fulfill({ status: options.proxyFailure ? 503 : 200, contentType: 'application/json', body: JSON.stringify({ proxies: state.proxies }) })
    if (!url.pathname.startsWith('/api/admin/oauth-gateway/')) return route.fulfill({ contentType: 'text/html', body: '<!doctype html><html><head><meta name="viewport" content="width=device-width,initial-scale=1"></head><body><div id="root"></div></body></html>' })
    assert.equal(request.headers()['x-admin-key'], 'synthetic-admin')
    const p = url.pathname.slice('/api/admin/oauth-gateway'.length)
    state.requests.push({ path: p, method: request.method() })
    const send = (data, status = 200) => route.fulfill({ status, contentType: 'application/json', body: JSON.stringify({ ok: true, data }) })
    const fail = (code, status = 502) => route.fulfill({ status, contentType: 'application/json', body: JSON.stringify({ ok: false, error: { code, message: `Synthetic ${code}` } }) })
    const resource = p.slice(1)
    if (state.failures[resource] > 0) { state.failures[resource]--; return fail(`${resource}_failed`) }
    if (p === '/settings') {
      if (request.method() === 'PUT') state.settings.oauth_url = request.postDataJSON().oauth_url
      return send(state.settings)
    }
    if (p === '/capabilities') return send(state.caps)
    if (p === '/quota') return send(state.quota)
    if (p === '/groups') return send(state.groups)
    if (p === '/connections/test') return send({ oauth: { ok: true, capabilities: state.caps, quota: state.quota }, codex: { ok: true, groups: state.groups } })
    if (p === '/recovery') return send(state.recoveries)
    if (/^\/recovery\/\d+\/disable$/.test(p)) {
      const id = Number(p.split('/')[2]); state.disabledRecoveries.push(id)
      state.recoveries = state.recoveries.map(entry => entry.account_id === id ? { ...entry, state: 'disabled', can_disable: false } : entry)
      return send({})
    }
    if (p === '/preview') {
      state.previewInput = request.postDataJSON()
      return send({ valid: true, count: 1, groups: 1, rows: [{ line: 1, identifier: 'fixture@example.invalid', identifier_masked: 'f***@example.invalid', proxy_mode: 'system' }], errors: [] })
    }
    if (p === '/jobs' && request.method() === 'POST') {
      state.submissions++
      state.lastInput = request.postDataJSON()
      state.job = { id: state.lastInput.request_id, state: 'running', total: 1, imported: 0, failed: 0, pending: 1, can_refresh: true, can_cancel: true, can_retry_import: false, can_archive: false, retained_material: false, archived: false, recovery_account_id: state.lastInput.recovery_account_id, groups: [{ index: 0, state: 'running', batch_id: 'gb_fixture', request_id: 'bridge_fixture', imported: 0, total: 1, items: [{ line: 1, identifier: 'fixture@example.invalid', identifier_masked: 'f***@example.invalid', state: 'running', import_state: 'pending' }] }] }
      if (options.uncertain) return fail('synthetic_response_lost', 503)
      return send(state.job, 202)
    }
    if (p === '/jobs') return send(url.searchParams.has('archived') ? state.archived : [...state.jobs, ...(state.job ? [state.job] : [])].filter(job => !job.archived))
    if (p.endsWith('/archive') && request.method() === 'POST') {
      const job = [...state.jobs, state.job].find(job => job && p === `/jobs/${job.id}/archive`)
      if (!job?.can_archive) return fail('cannot_archive', 409)
      job.archived = true; state.archivedIDs.push(job.id)
      const summary = { ...job, groups: [] }
      state.archived.push(summary)
      return send(summary)
    }
    if (p.startsWith('/jobs/') && request.method() === 'GET') {
      state.lookups.push(p.slice('/jobs/'.length))
      return state.missingLookup ? fail('not_found', 404) : send(state.job)
    }
    throw new Error(`unhandled fixture path: ${request.method()} ${p}`)
  })
  await page.goto(origin)
  await page.evaluate(noMaskSupport => {
    localStorage.setItem('lang', 'zh')
    if (noMaskSupport) CSS.supports = () => false
  }, !!options.noMaskSupport)
  await page.addStyleTag({ content: css })
  await page.addScriptTag({ content: script })
  await page.getByRole('heading', { name: 'OAuth 网关', exact: true }).waitFor().catch(async error => {
    const headings = await page.getByRole('heading').allTextContents()
    throw new Error(`${error.message}\nHeadings: ${JSON.stringify(headings)}; browser errors: ${JSON.stringify(errors)}`)
  })
  return { page, state }
}
async function ready(page) {
  await page.getByRole('button', { name: '读取能力和配额', exact: true }).waitFor()
  await page.waitForFunction(() => [...document.querySelectorAll('button')].some(b => b.textContent === '读取能力和配额' && !b.disabled))
}
async function prepare(page) {
  await ready(page)
  await page.getByLabel('账号材料', { exact: true }).fill(material)
  await page.getByRole('button', { name: '预检', exact: true }).click()
  await page.getByText(/^预检通过 ·/).waitFor()
}

test('initial load, full identifiers, preview invalidation, terminal reasons and archive', async t => {
  const { page, state } = await fixture(t)
  const submit = page.getByRole('button', { name: '创建授权任务', exact: true })
  assert.equal(await submit.isDisabled(), true)
  await prepare(page)
  assert.equal(state.requests.filter(r => r.path === '/capabilities').length, 1)
  assert.equal(state.requests.filter(r => r.path === '/groups').length, 1)
  assert.equal(await page.getByLabel('测试分组', { exact: true }).count(), 1)
  assert.equal(await page.getByText('1. fixture@example.invalid · system', { exact: true }).count(), 1)
  assert.equal(await page.getByText(/f\*\*\*@example/).count(), 0)
  await page.getByLabel('时区（可选）').fill('UTC')
  assert.equal(await submit.isDisabled(), true)
  await page.getByText('材料或选项已修改，预检已失效，请重新预检。', { exact: true }).waitFor()
  await page.getByRole('button', { name: '预检', exact: true }).click()
  await page.getByText(/^预检通过 ·/).waitFor()
  await submit.click()
  await page.getByText(/任务已接受：/).waitFor()
  assert.equal(state.submissions, 1)
  assert.equal(state.lastInput.save_credentials, false)
  assert.ok(state.lastInput.source_text.includes('----  synthetic-password  ----'))
  assert.equal(await page.getByLabel('账号材料', { exact: true }).inputValue(), '')
  assert.equal(await page.evaluate(key => sessionStorage.getItem(key), pendingKey), null)
  await page.getByText('批次与账号详情', { exact: true }).click()
  state.job = { ...state.job, state: 'completed', imported: 1, pending: 0, can_refresh: false, can_cancel: false, can_archive: true, groups: [{ ...state.job.groups[0], state: 'complete', imported: 1, items: [{ line: 1, identifier: 'fixture@example.invalid', state: 'succeeded', import_state: 'imported' }] }] }
  await page.getByRole('button', { name: '刷新列表', exact: true }).click()
  await page.locator('details[open]').getByText(/授权成功/).waitFor()
  assert.equal(await page.getByRole('button', { name: '核查原任务', exact: true }).count(), 0)
  await page.screenshot({ path: path.join(artifacts, 'oauth-desktop.png'), fullPage: true })
  page.once('dialog', dialog => dialog.accept())
  await page.getByRole('button', { name: '归档（保留自动恢复）', exact: true }).click()
  await page.getByRole('button', { name: '归档任务', exact: true }).click()
  await page.getByText(/已归档/).waitFor()
  assert.equal(state.submissions, 1)
  assert.equal(await page.evaluate(() => document.documentElement.scrollWidth <= innerWidth), true)
})

test('quota failure leaves capabilities and groups usable, and retries only quota', async t => {
  const { page, state } = await fixture(t, { quotaFailures: 1 })
  await ready(page)
  await page.getByRole('alert').getByText(/quota_failed/).waitFor()
  assert.equal(await page.getByLabel('授权出口国家／地区', { exact: true }).isDisabled(), false)
  assert.equal(await page.getByLabel('测试分组', { exact: true }).count(), 1)
  await page.getByLabel('账号材料', { exact: true }).fill(material)
  assert.equal(await page.getByRole('button', { name: '预检', exact: true }).isDisabled(), false)
  await page.getByRole('button', { name: '重试读取配额', exact: true }).click()
  await page.getByText('配额剩余 / 总计: 9 / 10', { exact: true }).waitFor()
  assert.equal(state.requests.filter(r => r.path === '/quota').length, 2)
  assert.equal(state.requests.filter(r => r.path === '/capabilities').length, 1)
  assert.equal(state.requests.filter(r => r.path === '/groups').length, 1)
})

test('capability failure shows a local action, connection test reuses successful data', async t => {
  const { page, state } = await fixture(t, { capabilityFailures: 1 })
  await ready(page)
  await page.getByLabel('账号材料', { exact: true }).fill(material)
  assert.equal(await page.getByRole('button', { name: '预检', exact: true }).isDisabled(), true)
  await page.getByText('服务能力尚未读取，请在上方重试读取能力。', { exact: true }).waitFor()
  await page.getByRole('button', { name: '测试连接', exact: true }).click()
  await page.getByText('OAuth: OK; 本地分组: OK', { exact: true }).waitFor()
  await ready(page)
  assert.equal(await page.getByRole('button', { name: '预检', exact: true }).isDisabled(), false)
  assert.equal(state.requests.filter(r => r.path === '/capabilities').length, 1)
  assert.equal(await page.getByRole('button', { name: '重试读取能力', exact: true }).count(), 0)
})

test('refresh preserves selected groups, options, material and an unchanged valid preview', async t => {
  const { page, state } = await fixture(t)
  await ready(page)
  await page.getByLabel('测试分组', { exact: true }).check()
  await page.getByLabel('授权出口国家／地区', { exact: true }).click()
  await page.getByRole('option', { name: '美国 (US)', exact: true }).click()
  await page.getByLabel('时区（可选）').fill('UTC')
  await prepare(page)
  await page.getByRole('button', { name: '读取能力和配额', exact: true }).click()
  await ready(page)
  assert.equal(await page.getByLabel('测试分组', { exact: true }).isChecked(), true)
  assert.equal(await page.getByLabel('时区（可选）').inputValue(), 'UTC')
  assert.equal(await page.getByLabel('账号材料', { exact: true }).inputValue(), material)
  assert.equal(await page.getByRole('button', { name: '创建授权任务', exact: true }).isDisabled(), false)
  await page.getByRole('button', { name: '创建授权任务', exact: true }).click()
  await page.getByText(/任务已接受：/).waitFor()
  assert.deepEqual(state.lastInput.group_ids, [1])
  assert.equal(state.lastInput.options.country, 'US')
  assert.equal(state.lastInput.options.timezone, 'UTC')
})

test('empty groups are optional, multiline plaintext works without WebkitTextSecurity', async t => {
  const { page, state } = await fixture(t, { emptyGroups: true, noMaskSupport: true })
  await ready(page)
  await page.getByText('暂无 Codex 分组，可以不分组继续。', { exact: true }).waitFor()
  const input = page.getByLabel('账号材料', { exact: true })
  await input.fill(material)
  assert.notEqual(await input.evaluate(el => getComputedStyle(el).webkitTextSecurity), 'disc')
  await page.getByRole('button', { name: '预检', exact: true }).click()
  await page.getByText(/^预检通过 ·/).waitFor()
  assert.deepEqual(state.previewInput.group_ids, [])
  assert.equal(await page.getByRole('button', { name: '创建授权任务', exact: true }).isDisabled(), false)
})

test('group failure is not presented as an empty list and can be retried separately', async t => {
  const { page, state } = await fixture(t, { groupFailures: 1 })
  await ready(page)
  await page.getByText('分组读取失败，请在下方重试。', { exact: true }).waitFor()
  await page.getByRole('button', { name: '重试读取分组', exact: true }).click()
  await page.getByLabel('测试分组', { exact: true }).waitFor()
  assert.equal(state.requests.filter(r => r.path === '/groups').length, 2)
  assert.equal(state.requests.filter(r => r.path === '/quota').length, 1)
})

test('repair is explicit, opts into retention, and cannot change existing destination bindings', async t => {
  const { page, state } = await fixture(t)
  await ready(page)
  const optIn = page.getByLabel('成功后启用 401 自动恢复（加密保管密码和 TOTP）', { exact: true })
  assert.equal(await optIn.isChecked(), false)
  await page.getByLabel('测试分组', { exact: true }).check()
  await page.getByLabel('本次批量运行代理 URI', { exact: true }).fill('socks5://synthetic.example.invalid:1080')
  await page.getByLabel(/修复已有账号 ID/).fill('42')
  await optIn.check()
  assert.equal(await page.getByLabel('测试分组', { exact: true }).isDisabled(), true)
  assert.equal(await page.getByLabel('本次批量运行代理 URI', { exact: true }).isDisabled(), true)
  await prepare(page)
  await page.getByRole('button', { name: '授权并修复原账号', exact: true }).click()
  await page.getByText(/任务已接受：/).waitFor()
  assert.equal(state.lastInput.recovery_account_id, 42)
  assert.equal(state.lastInput.save_credentials, true)
  assert.deepEqual(state.lastInput.group_ids, [])
  assert.equal('runtime_proxy' in state.lastInput, false)
  assert.equal(state.submissions, 1)
})

test('recovery stop is explicit and the state remains visible', async t => {
  const { page, state } = await fixture(t, { recoveries: [{ account_id: 42, identifier: 'fixture@example.invalid', state: 'watching', attempts: 0, can_disable: true }] })
  const stop = page.getByRole('button', { name: '停止恢复并清除材料', exact: true })
  await stop.waitFor()
  page.once('dialog', dialog => dialog.accept())
  await stop.click()
  await page.getByText('这条记录已停止恢复。', { exact: true }).waitFor()
  assert.deepEqual(state.disabledRecoveries, [42])
  assert.equal(await stop.isDisabled(), true)
})

test('uncertain submission survives navigation, missing lookup never releases the duplicate guard', async t => {
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
  state.missingLookup = true
  await page.getByRole('button', { name: '查找原任务', exact: true }).click()
  await page.getByRole('alert').getByText(/not_found/).waitFor()
  assert.equal(await page.getByRole('button', { name: '创建授权任务', exact: true }).isDisabled(), true)
  state.missingLookup = false
  await page.getByRole('button', { name: '查找原任务', exact: true }).click()
  await page.getByText(/已找到原任务：/).waitFor()
  assert.deepEqual(state.lookups, [originalID, originalID])
  assert.equal(state.submissions, 1)
  assert.equal(await page.evaluate(key => sessionStorage.getItem(key), pendingKey), null)
})

test('a stalled response body times out as uncertain without retrying creation', async t => {
  const { page } = await fixture(t)
  await ready(page)
  const result = await page.evaluate(async () => {
    const originalFetch = window.fetch, originalTimer = window.setTimeout, originalClear = window.clearTimeout
    let expire, calls = 0
    window.setTimeout = (fn, delay, ...args) => delay === 35000 ? (expire = fn, -1) : originalTimer(fn, delay, ...args)
    window.clearTimeout = id => { if (id !== -1) originalClear(id) }
    window.fetch = async (_url, options) => {
      calls++
      return { status: 202, ok: true, json: () => new Promise((_resolve, reject) => {
        options.signal.addEventListener('abort', () => reject(new DOMException('Aborted', 'AbortError')), { once: true })
        queueMicrotask(() => expire())
      }) }
    }
    try {
      await window.__gatewayRequest('/jobs', 'POST', { request_id: 'synthetic-timeout' })
      return { unexpectedSuccess: true }
    } catch (error) { return { calls, uncertain: error.uncertain, code: error.problem?.code } }
    finally { window.fetch = originalFetch; window.setTimeout = originalTimer; window.clearTimeout = originalClear }
  })
  assert.deepEqual(result, { calls: 1, uncertain: true, code: 'request_timeout' })
})

test('mobile layout fits and exposes keyboard-operable controls', async t => {
  const { page } = await fixture(t, { mobile: true })
  await ready(page)
  await page.getByRole('button', { name: '读取能力和配额', exact: true }).focus()
  await page.keyboard.press('Enter')
  await ready(page)
  assert.equal(await page.evaluate(() => document.documentElement.scrollWidth <= innerWidth), true)
  await page.screenshot({ path: path.join(artifacts, 'oauth-mobile.png'), fullPage: true })
})

test('recovery opt-in is blocked without the original-identity guard', async t => {
  const { page, state } = await fixture(t)
  await ready(page)
  state.caps.features.expected_identity_guard = false
  await page.getByRole('button', { name: '读取能力和配额', exact: true }).click()
  await ready(page)
  await page.getByLabel('账号材料', { exact: true }).fill(material)
  const optIn = page.getByLabel('成功后启用 401 自动恢复（加密保管密码和 TOTP）', { exact: true })
  await optIn.check()
  await page.getByText('网关未提供原身份校验能力，不能修复原账号或启用自动恢复。', { exact: true }).waitFor()
  assert.equal(await page.getByRole('button', { name: '预检', exact: true }).isDisabled(), true)
  await optIn.uncheck()
  assert.equal(await page.getByRole('button', { name: '预检', exact: true }).isDisabled(), false)
  assert.equal(state.submissions, 0)
})

test('one account can select a runtime proxy without changing OAuth proxy mode', async t => {
  const { page, state } = await fixture(t)
  await prepare(page)
  assert.equal(await page.getByRole('button', { name: 'OAuth 代理模式' }).textContent(), '网关动态代理')
  await page.getByText('单账号覆盖 (0)', { exact: true }).click()
  const row = page.getByRole('group', { name: '账号 fixture@example.invalid 的运行代理', exact: true })
  await row.getByRole('button', { name: '从代理池选择', exact: false }).click()
  await page.getByRole('option').filter({ hasText: '运行节点一' }).click()
  assert.equal(await row.getByLabel('账号 fixture@example.invalid 的运行代理 URI', { exact: true }).inputValue(), state.proxies[0].url)
  assert.equal(await page.getByRole('button', { name: '创建授权任务', exact: true }).isDisabled(), true)
  await page.getByRole('button', { name: '预检', exact: true }).click()
  await page.getByText(/^预检通过 ·/).waitFor()
  assert.equal(state.previewInput.accounts[0].runtime_proxy, state.proxies[0].url)
  assert.equal(state.previewInput.options.proxy_mode, 'system')
  assert.equal(state.previewInput.options.proxy, undefined)
  await page.screenshot({ path: path.join(artifacts, 'oauth-account-proxy-desktop.png'), fullPage: true })
  await page.getByRole('button', { name: '创建授权任务', exact: true }).click()
  await page.getByText(/任务已接受：/).waitFor()
  assert.equal(state.lastInput.accounts[0].runtime_proxy, state.proxies[0].url)
  const stored = await page.evaluate(() => JSON.stringify({ ...sessionStorage, ...localStorage }))
  assert.equal(stored.includes('one-runtime'), false)
})

test('runtime proxy probes show latency without changing the preview or binding', async t => {
  const { page, state } = await fixture(t)
  await prepare(page)
  const batch = page.getByRole('button', { name: '测试批量代理连通性', exact: true })
  assert.equal(await batch.isDisabled(), true)
  await page.getByLabel('本次批量运行代理 URI', { exact: true }).fill(state.proxies[0].url)
  await page.getByRole('button', { name: '预检', exact: true }).click()
  await page.getByText(/^预检通过 ·/).waitFor()
  await batch.click()
  await page.getByText('可连通 · 123 ms', { exact: true }).waitFor()
  assert.equal(await page.getByRole('button', { name: '创建授权任务', exact: true }).isDisabled(), false)
  await page.getByText('单账号覆盖 (0)', { exact: true }).click()
  const row = page.getByRole('group', { name: '账号 fixture@example.invalid 的运行代理', exact: true })
  const probe = row.getByRole('button', { name: /测试账号 .* 的代理连通性/ })
  await probe.click()
  await row.getByText('可连通 · 123 ms', { exact: true }).waitFor()
  await row.getByLabel('账号 fixture@example.invalid 的运行代理 URI', { exact: true }).fill(state.proxies[1].url)
  assert.equal(await row.getByRole('status').count(), 0)
  await probe.click()
  await row.getByText('不可连通', { exact: true }).waitFor()
  assert.deepEqual(state.proxyTests, [state.proxies[0].url, state.proxies[0].url, state.proxies[1].url].map(url => ({ url, lang: 'zh-CN' })))
  assert.equal(state.submissions, 0)
})

test('multiple account runtime proxies survive reordering and inherit the batch default', async t => {
  const { page, state } = await fixture(t)
  await ready(page)
  const first = material, second = material.replace('fixture@', 'second@')
  await page.getByLabel('账号材料', { exact: true }).fill(`${first}\n${second}`)
  await page.getByLabel('本次批量运行代理 URI', { exact: true }).fill(state.proxies[0].url)
  await page.getByText('单账号覆盖 (0)', { exact: true }).click()
  await page.getByLabel('账号 second@example.invalid 的运行代理 URI', { exact: true }).fill(state.proxies[1].url)
  await page.getByLabel('账号材料', { exact: true }).fill(`${second}\n${first}`)
  assert.equal(await page.getByLabel('账号 second@example.invalid 的运行代理 URI', { exact: true }).inputValue(), state.proxies[1].url)
  assert.equal(await page.getByLabel('账号 fixture@example.invalid 的运行代理 URI', { exact: true }).inputValue(), '')
  await page.getByRole('button', { name: '预检', exact: true }).click()
  await page.getByText(/^预检通过 ·/).waitFor()
  assert.equal(state.previewInput.runtime_proxy, state.proxies[0].url)
  assert.deepEqual(state.previewInput.accounts.map(a => [a.identifier, a.runtime_proxy || '']), [['second@example.invalid', state.proxies[1].url], ['fixture@example.invalid', '']])
  await page.getByRole('button', { name: '读取能力和配额', exact: true }).click()
  await ready(page)
  assert.equal(await page.getByRole('button', { name: '创建授权任务', exact: true }).isDisabled(), false)
  await page.getByRole('button', { name: '创建授权任务', exact: true }).click()
  await page.getByText(/任务已接受：/).waitFor()
  assert.equal(state.lastInput.accounts[0].runtime_proxy, state.proxies[1].url)
})

test('JSON runtime pins can be reset to inherit and are omitted from account repair', async t => {
  const { page, state } = await fixture(t)
  await ready(page)
  await page.getByLabel('输入格式', { exact: true }).click()
  await page.getByRole('option', { name: 'JSON', exact: true }).click()
  await page.getByLabel('账号材料', { exact: true }).fill(JSON.stringify([{ identifier: 'fixture@example.invalid', password: 'synthetic-password', totp_secret: 'JBSWY3DPEHPK3PXP', proxy: 'http://oauth-only.example.invalid:8080', runtime_proxy: state.proxies[1].url }]))
  await page.getByText('单账号覆盖 (1)', { exact: true }).click()
  const row = page.getByRole('group', { name: '账号 fixture@example.invalid 的运行代理', exact: true })
  assert.equal(await row.getByLabel('账号 fixture@example.invalid 的运行代理 URI', { exact: true }).inputValue(), state.proxies[1].url)
  await row.getByRole('button', { name: '沿用统一设置', exact: true }).click()
  assert.equal(await row.getByLabel('账号 fixture@example.invalid 的运行代理 URI', { exact: true }).inputValue(), '')
  await row.getByLabel('账号 fixture@example.invalid 的运行代理 URI', { exact: true }).fill(state.proxies[0].url)
  await page.getByLabel(/修复已有账号 ID/).fill('42')
  assert.equal(await row.getByLabel('账号 fixture@example.invalid 的运行代理 URI', { exact: true }).isDisabled(), true)
  await page.getByRole('button', { name: '预检', exact: true }).click()
  await page.getByText(/^预检通过 ·/).waitFor()
  assert.equal(state.previewInput.accounts[0].runtime_proxy, undefined)
  assert.equal(state.previewInput.accounts[0].proxy, 'http://oauth-only.example.invalid:8080')
})

test('proxy list failures remain visible and mobile account proxy controls fit', async t => {
  const { page, state } = await fixture(t, { mobile: true, proxyFailure: true })
  await ready(page)
  await page.getByLabel('账号材料', { exact: true }).fill(material)
  await page.getByText('Synthetic proxy list failed', { exact: true }).waitFor()
  await page.getByText('单账号覆盖 (0)', { exact: true }).click()
  await page.getByLabel('账号 fixture@example.invalid 的运行代理 URI', { exact: true }).fill(state.proxies[0].url)
  assert.equal(await page.getByRole('button', { name: '预检', exact: true }).isDisabled(), false)
  assert.equal(await page.evaluate(() => document.documentElement.scrollWidth <= innerWidth), true)
  await page.screenshot({ path: path.join(artifacts, 'oauth-account-proxy-mobile.png'), fullPage: true })
})

test('reauthorization requires fresh matching single-account material, preview and confirmation', async t => {
  const { page, state } = await fixture(t)
  await ready(page)
  state.job = { id: 'original-job', state: 'failed', total: 2, imported: 0, failed: 2, pending: 0, archived: false, groups: [{ index: 1, state: 'failed', imported: 0, total: 1, items: [{ line: 42, identifier: 'fixture@example.invalid', state: 'failed', import_state: 'not_ready', can_reauthorize: true, diagnostic: { upstream_code: 'invalid_state', http_status: 409, endpoint: 'api_accounts_mfa_verify', stage: 'totp_verify' } }] }] }
  await page.getByRole('button', { name: '刷新列表', exact: true }).click()
  await page.getByText('批次与账号详情', { exact: true }).click()
  await page.getByText(/未导入（授权失败）/).waitFor()
  await page.getByText(/登录会话已失效/).waitFor()
  await page.getByText(/HTTP 409 · 阶段: TOTP 验证 · 接口: api_accounts_mfa_verify/).waitFor()
  await page.getByLabel('账号材料', { exact: true }).fill('other----p----t')
  page.once('dialog', dialog => dialog.dismiss())
  await page.getByRole('button', { name: '重新授权此账号（新任务）', exact: true }).click()
  assert.equal(await page.getByLabel('账号材料', { exact: true }).inputValue(), 'other----p----t')
  page.once('dialog', dialog => dialog.accept())
  await page.getByRole('button', { name: '重新授权此账号（新任务）', exact: true }).click()
  assert.equal(await page.getByLabel('账号材料', { exact: true }).inputValue(), '')
  await page.getByLabel('账号材料', { exact: true }).fill('other----p----t')
  assert.equal(await page.getByRole('button', { name: '预检', exact: true }).isDisabled(), true)
  await page.getByLabel('账号材料', { exact: true }).fill(`${material}\nother----p----t`)
  assert.equal(await page.getByRole('button', { name: '预检', exact: true }).isDisabled(), true)
  await page.getByLabel('账号材料', { exact: true }).fill(material)
  await page.getByRole('button', { name: '预检', exact: true }).click()
  await page.getByText(/^预检通过 ·/).waitFor()
  assert.deepEqual(state.previewInput.retry_of, { job_id: 'original-job', line: 42 })
  assert.equal(state.previewInput.recovery_account_id, undefined)
  page.once('dialog', dialog => dialog.dismiss())
  await page.getByRole('button', { name: '创建授权任务', exact: true }).click()
  assert.equal(state.submissions, 0)
  page.once('dialog', dialog => dialog.accept())
  await page.getByRole('button', { name: '创建授权任务', exact: true }).click()
  await page.getByText(/任务已接受：/).waitFor()
  assert.equal(state.submissions, 1)
  assert.deepEqual(state.lastInput.retry_of, { job_id: 'original-job', line: 42 })
})

test('job filters, stable pagination, result snapshot and guarded batch archive', async t => {
  const active = { id: 'active-review', job_type: 'auto_recovery', state: 'needs_review', total: 1, imported: 0, failed: 0, pending: 1, can_refresh: true, can_archive: false, group_ids: [1] }
  const jobs = Array.from({ length: 12 }, (_, i) => ({
    id: `fixture-job-${i}`, job_type: 'import', state: 'completed', total: 1, imported: 1, failed: 0, pending: 0, can_archive: true, group_ids: i === 9 ? [1] : [],
    created_at: `2026-01-${String(20 - i).padStart(2, '0')}T00:00:00Z`, requested_country: 'US',
    account_results: [{ line: 1, account_id: 100 + i, plan_type: 'plus', workspace_id: 'synthetic-workspace', workspace_name: '研发团队', group_ids: i === 9 ? [1] : [], runtime_proxy_id: 2, recovery_state: 'ready', identifier_masked: 'f***@example.invalid', outcome: 'imported' }],
    groups: [{ index: 0, state: 'completed', imported: 1, total: 1, items: [{ line: 1, identifier: `fixture-${i}@example.invalid`, state: 'succeeded', import_state: 'imported' }] }],
  }))
  const { page, state } = await fixture(t, { jobs: [...jobs, active], recoveries: [{ account_id: 109, identifier: 'fixture@example.invalid', state: 'ready', attempts: 0, can_disable: true, account_disabled: true, last_outcome: 'refresh_succeeded' }] })
  const panel = page.locator('#oauth-jobs')
  await panel.locator('article').first().getByText('active-review', { exact: true }).waitFor()
  assert.equal(await panel.locator('article').count(), 10)
  await panel.getByRole('button', { name: '下一页' }).click()
  const row = panel.locator('#oauth-job-fixture-job-9')
  await row.getByText('批次与账号详情', { exact: true }).click()
  await row.getByText('plus', { exact: true }).waitFor()
  assert.equal(await row.getByRole('link', { name: '#109', exact: true }).getAttribute('href'), '/accounts?account_id=109')
  await row.getByText('研发团队 · synthetic-workspace', { exact: true }).waitFor()
  assert.equal(await row.getByRole('link', { name: '#109', exact: true }).getAttribute('target'), '_blank')
  await panel.getByRole('button', { name: '刷新列表' }).click()
  await panel.getByText('第 2 / 2 页', { exact: true }).waitFor()
  assert.equal(await row.locator('details[open]').count(), 1)
  await panel.getByLabel('任务类型', { exact: true }).click()
  await page.getByRole('option', { name: '新增导入', exact: true }).click()
  await panel.getByLabel('任务目标分组', { exact: true }).click()
  await page.getByRole('option', { name: '测试分组', exact: true }).click()
  assert.equal(await panel.locator('article').count(), 1)
  await panel.getByLabel('搜索任务或账号').fill('#109')
  await row.getByText('plus', { exact: true }).waitFor()
  const dialog = page.waitForEvent('dialog').then(async dialog => { assert.match(dialog.message(), /自动恢复不受影响/); await dialog.accept() })
  await panel.getByRole('button', { name: '归档本页已结束任务（1）', exact: true }).click()
  await dialog
  await page.getByText('已归档 1/1 个任务。', { exact: true }).waitFor()
  assert.deepEqual(state.archivedIDs, ['fixture-job-9'])
  await panel.getByRole('button', { name: '归档任务', exact: true }).click()
  await row.getByText('plus', { exact: true }).waitFor()
  assert.equal(await row.getByText('fixture-9@example.invalid', { exact: true }).count(), 0)
  await page.getByRole('link', { name: '账号自动恢复', exact: true }).click()
  await page.locator('#oauth-recovery').getByText(/普通令牌刷新成功/).waitFor()
  await page.locator('#oauth-recovery').getByText(/账号已禁用；恢复不会自动启用账号/).waitFor()
  assert.equal(await page.evaluate(() => location.hash), '#oauth-recovery')
  assert.equal(state.submissions, 0)
})
