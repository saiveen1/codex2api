import assert from 'node:assert/strict'
import { after, before, test } from 'node:test'
import path from 'node:path'
import { fileURLToPath } from 'node:url'
import { mkdir, mkdtemp } from 'node:fs/promises'
import { tmpdir } from 'node:os'
import { build } from 'vite'
import react from '@vitejs/plugin-react'
import tailwindcss from '@tailwindcss/vite'
import { chromium } from 'playwright'

const root = fileURLToPath(new URL('../', import.meta.url))
let browser, script, css, artifactDir

before(async () => {
  artifactDir = process.env.IMAGE_CHANNEL_TEST_ARTIFACT_DIR || await mkdtemp(path.join(tmpdir(), 'codex2api-image-channel-'))
  await mkdir(artifactDir, { recursive: true })
  const result = await build({ configFile: false, root, logLevel: 'silent', plugins: [react(), tailwindcss(), {
    name: 'image-channel-harness', enforce: 'pre',
    resolveId(id) { if (id === 'virtual:image-channel-harness') return '\0image-channel-harness' },
    load(id) {
      if (id !== '\0image-channel-harness') return
      return `import React from 'react'; import {createRoot} from 'react-dom/client';
        import Studio from ${JSON.stringify(path.join(root, 'src/pages/ImageStudio.tsx'))};
        import Portal from ${JSON.stringify(path.join(root, 'src/pages/ImageStudioPortal.tsx'))};
        import {ToastProvider} from ${JSON.stringify(path.join(root, 'src/components/ToastProvider.tsx'))};
        import {BrandingProvider} from ${JSON.stringify(path.join(root, 'src/branding.tsx'))};
        import {ThemeProvider} from ${JSON.stringify(path.join(root, 'src/hooks/useTheme.tsx'))};
        import {MemoryRouter} from 'react-router-dom';
        import ${JSON.stringify(path.join(root, 'src/i18n.ts'))}; import ${JSON.stringify(path.join(root, 'src/index.css'))};
        createRoot(document.getElementById('root')).render(
          React.createElement(MemoryRouter,null,React.createElement(ThemeProvider,null,React.createElement(BrandingProvider,null,
            React.createElement(ToastProvider,null,React.createElement(window.__portal ? Portal : Studio))))));`
    },
  }], resolve: { alias: { '@': path.join(root, 'src') } }, define: { 'process.env.NODE_ENV': '"production"' }, build: {
    write: false, minify: false, rollupOptions: { input: 'virtual:image-channel-harness' },
    lib: { entry: 'virtual:image-channel-harness', formats: ['iife'], name: 'ImageChannelHarness' },
  } })
  const bundle = Array.isArray(result) ? result[0] : result
  script = bundle.output.find(item => item.type === 'chunk').code
  css = bundle.output.filter(item => item.type === 'asset' && item.fileName.endsWith('.css')).map(item => item.source).join('\n')
  browser = await chromium.launch({ headless: true, executablePath: process.env.CHROMIUM_PATH || undefined, args: ['--no-sandbox'] })
})
after(async () => { await browser?.close() })

async function fixture(t, { webKey = true, serviceReady = true, portal = false, metadataReady = false, modelAllow = null, cancelMode = '', viewport = { width: 1440, height: 1000 } } = {}) {
  const page = await browser.newPage({ viewport })
  page.setDefaultTimeout(5000)
  const errors = []
  page.on('pageerror', error => errors.push(error.message))
  t.after(() => assert.deepEqual(errors, []))
  t.after(() => page.close())
  await page.route('http://image.test/**', route => route.fulfill({ contentType: 'text/html', body: '<div id="root"></div>' }))
  await page.goto('http://image.test/')
  await page.evaluate(({ webKey, serviceReady, portal, metadataReady, modelAllow, cancelMode }) => {
    localStorage.setItem('lang', 'zh')
    if (portal) localStorage.setItem('codex2api_image_studio_api_key', JSON.stringify({ key: 'sk-synthetic-web', remember: true }))
    window.__portal = portal
    window.__submissions = []
    window.__cancels = []
    let job = null
    const keys = [
      { id: 10, name: 'native-fixture', key: 'sk-native***', limits: { upstream_channel: 'codex' } },
      { id: 30, name: 'grok-fixture', key: 'sk-grok***', limits: { upstream_channel: 'grok' } },
      ...(webKey ? [{ id: 20, name: 'web-fixture', key: 'sk-web***', limits: { upstream_channel: 'chatgpt_web', ...(modelAllow ? { model_allow: modelAllow } : {}) } }] : []),
    ].map(key => ({ ...key, status: 'active', quota_limit: 0, quota_used: 0, total_used: 0, allowed_group_ids: [], created_at: '2026-01-01T00:00:00Z' }))
    window.fetch = async (url, options = {}) => {
      const pathname = new URL(String(url), location.origin).pathname
      let body = {}, status = 200
      if (cancelMode && pathname.endsWith('/jobs/42/cancel')) {
        window.__cancels.push({ pathname, authorization: new Headers(options.headers).get('Authorization'), method: options.method })
        await new Promise(resolve => { window.__releaseCancel = resolve })
        if (cancelMode === 'conflict') { status = 409; job = { ...job, status: 'succeeded' }; body = { error: 'already finished' } }
        else if (cancelMode === 'error') { status = 500; body = { error: 'synthetic cancel error' } }
        else { job = { ...job, status: 'cancelled' }; body = { status: 'cancelled' } }
      } else if (cancelMode && pathname.endsWith('/jobs/42')) body = { job }
      else if (pathname === '/api/admin/keys') body = { keys }
      else if (pathname === '/api/admin/chatgpt-web') body = { enabled: serviceReady, configured: serviceReady, supported_models: ['gpt-image-2', 'gpt-image-2.5', 'gpt-image-2.5-flare', 'gpt-image-2.5-sunburst', 'gpt-image-2.6', 'gpt-image-2.6-flare', 'gpt-image-2.6-sunburst'], protocol_version: 'fixture' }
      else if (pathname === '/api/admin/image-prompts') body = { templates: [] }
      else if (pathname.endsWith('/assets')) body = { assets: [], total: 0 }
      else if (pathname.endsWith('/quota')) body = { quota_limit: 0, quota_used: 0, quota_remaining: null, expires_at: null, status: 'active', refresh_after_seconds: 3600 }
      else if (pathname.startsWith('/api/key-usage/')) {
        if (metadataReady) body = { key: { limits: { upstream_channel: 'chatgpt_web', ...(modelAllow ? { model_allow: modelAllow } : {}) } } }
        else { status = 503; body = { error: 'Synthetic metadata failure' } }
      }
      else if (pathname.endsWith('/jobs') || pathname.endsWith('/edit-jobs')) {
        if (options.method === 'POST') {
          window.__submissions.push(JSON.parse(options.body))
          job = cancelMode ? { id: 42, status: cancelMode === 'queued' ? 'queued' : 'running', prompt: 'synthetic', assets: [], created_at: '2026-01-01T00:00:00Z' } : { id: 1, status: 'failed', error: 'Synthetic terminal result', outputs: [], prompt: 'synthetic', model: 'gpt-image-2', created_at: '2026-01-01T00:00:00Z' }
          body = { job }
        } else body = { jobs: cancelMode && job ? [job] : [], total: cancelMode && job ? 1 : 0 }
      } else if (options.method && options.method !== 'GET') throw new Error('Unexpected write: ' + pathname)
      return new Response(JSON.stringify(body), { status, headers: { 'Content-Type': 'application/json' } })
    }
  }, { webKey, serviceReady, portal, metadataReady, modelAllow, cancelMode })
  await page.addStyleTag({ content: css })
  await page.addScriptTag({ content: script })
  return page
}

async function chooseWeb(page) {
  await page.getByText('生图来源', { exact: true }).first().waitFor()
  await page.getByRole('button', { name: '生图来源', exact: true }).click()
  await page.getByRole('option', { name: /Web 生图（实验性）/ }).click()
}

test('Web without a dedicated key cannot fall back to automatic native routing', async t => {
  const page = await fixture(t, { webKey: false })
  await chooseWeb(page)
  await page.locator('#image-studio-prompt').fill('A synthetic red square')
  const submit = page.locator('.studio-submit-button')
  if (!await submit.isDisabled()) await submit.click()
  assert.equal(await page.evaluate(() => window.__submissions.length), 0)
  assert.equal(await page.getByRole('link', { name: '创建 API Key', exact: true }).isVisible(), true)
})

test('visible Web source uses only its dedicated key and safe parameters on desktop and mobile', async t => {
  const page = await fixture(t)
  await chooseWeb(page)
  await page.getByRole('button', { name: 'API Key', exact: true }).click()
  assert.equal(await page.getByRole('option').filter({ hasText: 'native-fixture' }).count(), 0)
  assert.equal(await page.getByRole('option').filter({ hasText: 'grok-fixture' }).count(), 0)
  await page.getByRole('option').filter({ hasText: 'web-fixture' }).click()
  await page.screenshot({ path: path.join(artifactDir, 'web-source-desktop.png'), fullPage: true })
  await page.setViewportSize({ width: 390, height: 844 })
  assert.equal(await page.getByRole('button', { name: '生图来源', exact: true }).isVisible(), true)
  assert.equal(await page.evaluate(() => document.documentElement.scrollWidth <= innerWidth + 1), true, 'mobile layout must not overflow')
  await page.screenshot({ path: path.join(artifactDir, 'web-source-mobile.png'), fullPage: true })
  await page.locator('#image-studio-prompt').fill('A synthetic red square')
  await page.locator('.studio-submit-button').click()
  await page.waitForFunction(() => window.__submissions.length === 1)
  const payload = await page.evaluate(() => window.__submissions[0])
  assert.equal(payload.api_key_id, 20)
  assert.equal(payload.model, 'gpt-image-2')
  assert.equal(payload.size, 'auto')
  assert.equal(payload.quality, 'auto')
  assert.equal(payload.background, 'auto')
})

test('portal refuses generation when current key channel metadata cannot be read', async t => {
  const page = await fixture(t, { portal: true })
  try { await page.getByText(/无法确认 Key 通道，禁止提交；请重试。/).waitFor() }
  catch (error) { console.error('PORTAL_FIXTURE_BODY', await page.locator('body').innerText()); throw error }
  await page.locator('textarea').first().fill('A synthetic red square')
  const submit = page.locator('.portal-generate')
  assert.equal(await submit.isDisabled(), true)
  assert.equal(await page.evaluate(() => window.__submissions.length), 0)
})

test('Web source clearly blocks submission until its service is configured', async t => {
  const page = await fixture(t, { serviceReady: false })
  await chooseWeb(page)
  await page.getByRole('button', { name: 'API Key', exact: true }).click()
  await page.getByRole('option').filter({ hasText: 'web-fixture' }).click()
  await page.locator('#image-studio-prompt').fill('A synthetic red square')
  await page.getByText(/Web 服务.*(未配置|尚未就绪)/).first().waitFor()
  assert.equal(await page.locator('.studio-submit-button').isDisabled(), true)
  assert.equal(await page.evaluate(() => window.__submissions.length), 0)
})

test('Web studio preserves a selected compatibility model in the submitted job', async t => {
  const page = await fixture(t)
  await chooseWeb(page)
  await page.getByRole('button', { name: 'API Key', exact: true }).click()
  await page.getByRole('option').filter({ hasText: 'web-fixture' }).click()
  await page.getByRole('button', { name: '模型', exact: true }).click()
  assert.equal(await page.getByRole('option').count(), 7)
  await page.getByRole('option', { name: 'gpt-image-2.6-flare', exact: true }).click()
  await page.locator('#image-studio-prompt').fill('A synthetic blue star')
  await page.locator('.studio-submit-button').click()
  await page.waitForFunction(() => window.__submissions.length === 1)
  assert.equal(await page.evaluate(() => window.__submissions[0].model), 'gpt-image-2.6-flare')
})

test('Web portal exposes only the current key model allowlist and preserves its alias', async t => {
  const page = await fixture(t, { portal: true, metadataReady: true, modelAllow: ['gpt-image-2.5-sunburst'] })
  await page.getByRole('button', { name: '模型', exact: true }).click()
  assert.equal(await page.getByRole('option').count(), 1)
  await page.getByRole('option', { name: 'gpt-image-2.5-sunburst', exact: true }).click()
  await page.locator('textarea').first().fill('A synthetic green square')
  await page.locator('.portal-generate').click()
  await page.waitForFunction(() => window.__submissions.length === 1)
  assert.equal(await page.evaluate(() => window.__submissions[0].model), 'gpt-image-2.5-sunburst')
})

for (const portal of [false, true]) {
  for (const mode of ['queued', 'success', 'conflict', 'error']) {
    test(`cancel ${portal ? 'portal' : 'admin'}: ${mode}`, async t => {
      const page = await fixture(t, { portal, metadataReady: true, cancelMode: mode })
      await page.locator(portal ? 'textarea' : '#image-studio-prompt').first().fill('synthetic')
      await page.locator(portal ? '.portal-generate' : '.studio-submit-button').click()
      const stop = page.getByRole('button', { name: '终止任务', exact: true })
      await stop.waitFor()
      await stop.click()
      await page.waitForFunction(() => window.__cancels.length === 1)
      assert.equal(await stop.isDisabled(), true)
      const calls = await page.evaluate(() => window.__cancels)
      assert.equal(calls[0].pathname, portal ? '/api/image-studio/jobs/42/cancel' : '/api/admin/images/jobs/42/cancel')
      assert.equal(calls[0].method, 'POST')
      if (portal) assert.equal(calls[0].authorization, 'Bearer sk-synthetic-web')
      await page.evaluate(() => window.__releaseCancel())
      if (mode === 'success' || mode === 'queued') await page.getByText('已终止', { exact: true }).first().waitFor()
      if (mode === 'conflict') await page.getByText('已完成', { exact: true }).first().waitFor()
      if (mode === 'error') { await page.getByText('synthetic cancel error').first().waitFor(); assert.equal(await stop.isEnabled(), true) }
      assert.equal(await page.evaluate(() => window.__cancels.length), 1)
    })
  }
}
