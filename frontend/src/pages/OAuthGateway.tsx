import { useEffect, useRef, useState, type ReactNode } from 'react'
import { Link } from 'react-router-dom'
import { useTranslation } from 'react-i18next'
import PageHeader from '../components/PageHeader'
import { Button } from '../components/ui/button'
import { Card, CardContent, CardHeader, CardTitle } from '../components/ui/card'
import { Input } from '../components/ui/input'
import { Select } from '../components/ui/select'
import { createSubmission, parseMaterial, previewMatches, readPendingRequest, rememberPendingRequest, forgetPendingRequest, type GatewayInput, type Problem } from '../lib/oauthGateway'
import { gatewayRequest, GatewayError } from '../lib/oauthGatewayApi'

type Settings = { oauth_url: string; oauth_token_set: boolean; configured: boolean; auto_recovery_supported: boolean; auto_recovery_reason: string }
type Capabilities = { countries?: string[]; environments?: string[]; proxy_modes?: string[]; workspace_policies?: string[]; max_batch_size?: number; max_upload_bytes?: number; features?: Record<string, boolean> }
type Quota = { remaining?: number; total?: number; reserved?: number; used?: number; disabled?: boolean }
type Group = { id: number; name: string }
type Identifier = { identifier?: string; identifier_masked?: string }
type Preview = { valid: boolean; count: number; groups: number; rows?: (Identifier & { line: number; proxy_mode: string })[]; errors?: { line: number; code: string }[] }
type Job = { id: string; state: string; total: number; imported: number; failed: number; pending: number; can_refresh: boolean; can_cancel: boolean; can_retry_import: boolean; can_archive: boolean; retained_material: boolean; archived: boolean; recovery_account_id?: number; auto_recovery?: boolean; groups?: { index: number; state: string; batch_id: string; request_id: string; imported: number; total: number; error?: Problem; read_operation?: string; read_failures?: number; next_check_at?: string; items?: (Identifier & { line: number; state: string; import_state: string; error?: Problem })[] }[] }
type Recovery = { account_id: number; identifier: string; state: string; attempts: number; next_attempt_at?: string; job_id?: string; error?: Problem; can_disable: boolean }
type Connection = { oauth: { ok: boolean; error?: Problem; capabilities?: Capabilities; quota?: Quota }; codex: { ok: boolean; error?: Problem; groups?: Group[] } }
type ProxyCheck = { usable: boolean; requested_country?: string; observed_country?: string; openai_reachable?: boolean; openai_http_status?: number; trace_http_status?: number; error?: Problem }
const fieldClass = 'w-full rounded-md border border-input bg-transparent px-3 py-2 text-sm outline-none focus-visible:ring-2 focus-visible:ring-ring'
const errorText = (e: unknown) => e instanceof Error ? e.message : 'Request failed'
const identifier = (item: Identifier) => item.identifier || item.identifier_masked || '—'
const label = (name: string, child: ReactNode, hint?: string) => <label className="flex min-w-0 flex-col gap-2 text-sm"><span className="font-medium">{name}</span>{child}{hint && <span className="text-xs text-muted-foreground">{hint}</span>}</label>

export default function OAuthGateway() {
  const { i18n } = useTranslation()
  const zh = i18n.resolvedLanguage?.startsWith('zh') ?? false
  const tr = (en: string, cn: string) => zh ? cn : en
  const [settings, setSettings] = useState<Settings | null>(null)
  const [url, setUrl] = useState('https://oauth.cala.lol')
  const [token, setToken] = useState('')
  const [caps, setCaps] = useState<Capabilities | null>(null)
  const capsRef = useRef<Capabilities | null>(null)
  const [quota, setQuota] = useState<Quota | null>(null)
  const [groups, setGroups] = useState<Group[] | null>(null)
  const [selectedGroups, setSelectedGroups] = useState<number[]>([])
  const [source, setSource] = useState('')
  const [format, setFormat] = useState<'txt' | 'json'>('txt')
  const [opts, setOpts] = useState<Record<string, string>>({ proxy_mode: 'system' })
  const [runtimeProxy, setRuntimeProxy] = useState('')
  const [saveCredentials, setSaveCredentials] = useState(false)
  const [recoveryAccountID, setRecoveryAccountID] = useState('')
  const [preview, setPreview] = useState<{ revision: number; input: GatewayInput; result: Preview } | null>(null)
  const [previewChanged, setPreviewChanged] = useState(false)
  const hadPreview = useRef(false)
  const [revision, setRevision] = useState(0)
  const revisionRef = useRef(0)
  const [uncertain, setUncertain] = useState<string | null>(readPendingRequest)
  const [pendingID, setPendingID] = useState('')
  const [jobs, setJobs] = useState<Job[] | null>(null)
  const [jobsError, setJobsError] = useState('')
  const [recoveries, setRecoveries] = useState<Recovery[] | null>(null)
  const [recoveryError, setRecoveryError] = useState('')
  const [archived, setArchived] = useState(false)
  const [reload, setReload] = useState(0)
  const [message, setMessage] = useState('')
  const [connection, setConnection] = useState('')
  const [proxyResult, setProxyResult] = useState('')
  const [pending, setPending] = useState<Record<string, boolean>>({})
  const [errors, setErrors] = useState<Record<string, string>>({})
  const busy = useRef(new Set<string>())
  const controller = useRef<AbortController | null>(null)
  const fileRef = useRef<HTMLInputElement>(null)
  const formLocked = !!pending.submit || !!uncertain
  const setupBusy = !!(pending.settings || pending.save || pending.test || pending.capabilities || pending.quota || pending.groups)
  const configLocked = setupBusy || formLocked || !!pending.preview
  const settingsDirty = !!settings && (url.trim() !== settings.oauth_url || token !== '')
  const repairing = recoveryAccountID !== ''
  const invalidGroups = groups ? selectedGroups.filter(id => !groups.some(g => g.id === id)) : []

  const problemText = (p?: Problem) => p ? [p.message || p.code, p.code && p.message ? `(${p.code})` : '', p.retry_after != null ? tr(`Retry after ${p.retry_after}s`, `${p.retry_after} 秒后再试`) : ''].filter(Boolean).join(' ') : ''
  const stateText = (state: string) => {
    const names: Record<string, string> = { completed: '已完成', complete: '已完成', queued: '排队中', checking: '检查中', submitting: '提交中', running: '处理中', partial: '部分完成', failed: '失败', cancelled: '已取消', manual_review: '待人工核查', recovering: '恢复中', refreshing: '正在刷新凭据', needs_review: '待人工核查', submit_unknown: '提交结果待核查', succeeded: '授权成功', imported: '已导入', import_failed: '入库失败，可重试', import_rejected: '拒绝入库，需核查', import_unknown: '入库结果待核查', not_ready: '等待授权结果', ready: '就绪', pending: '待处理', expired: '结果已过期', blocked: '已阻止', disabled: '已停用', cooldown: '冷却中', watching: '监测中', idle: '等待凭据失效', exhausted: '尝试次数已用尽' }
    return zh ? names[state] || state : state
  }
  const invalidate = () => {
    revisionRef.current++
    setRevision(revisionRef.current)
    setPreview(null)
    if (hadPreview.current) setPreviewChanged(true)
  }
  const clearMaterial = () => {
    setSource(''); setRuntimeProxy(''); setOpts(current => ({ ...current, proxy: '' }))
    if (fileRef.current) fileRef.current.value = ''
    invalidate(); hadPreview.current = false; setPreviewChanged(false)
  }
  const run = async (name: string, fn: (signal: AbortSignal) => Promise<void>) => {
    const owner = controller.current
    if (busy.current.has(name) || !owner || owner.signal.aborted) return
    busy.current.add(name); setPending(old => ({ ...old, [name]: true })); setErrors(old => ({ ...old, [name]: '' }))
    try { await fn(owner.signal) }
    catch (e) { if (!owner.signal.aborted) setErrors(old => ({ ...old, [name]: errorText(e) })) }
    finally {
      if (controller.current === owner) {
        busy.current.delete(name)
        if (!owner.signal.aborted) setPending(old => ({ ...old, [name]: false }))
      }
    }
  }
  const applyCapabilities = (value: Capabilities) => {
    if (capsRef.current && JSON.stringify(capsRef.current) !== JSON.stringify(value)) invalidate()
    capsRef.current = value; setCaps(value)
  }
  const loadCapabilities = () => run('capabilities', async signal => {
    const data = await gatewayRequest<Capabilities>('/capabilities', 'GET', undefined, signal)
    if (!signal.aborted) applyCapabilities(data)
  })
  const loadQuota = () => run('quota', async signal => {
    const data = await gatewayRequest<Quota>('/quota', 'GET', undefined, signal)
    if (!signal.aborted) setQuota(data)
  })
  const loadGroups = () => run('groups', async signal => {
    const data = await gatewayRequest<Group[]>('/groups', 'GET', undefined, signal)
    if (!signal.aborted) setGroups(data || [])
  })
  const loadSetup = () => Promise.all([loadCapabilities(), loadQuota(), loadGroups()])
  const loadSettings = () => run('settings', async signal => {
    const data = await gatewayRequest<Settings>('/settings', 'GET', undefined, signal)
    if (signal.aborted) return
    setSettings(data); setUrl(data.oauth_url || 'https://oauth.cala.lol')
    if (data.configured) await loadSetup()
    else await loadGroups()
  })
  useEffect(() => {
    const abort = new AbortController(); controller.current = abort; busy.current.clear()
    void loadSettings()
    return () => { abort.abort(); controller.current = null; revisionRef.current++ }
    // Initial load is bound to this mount's AbortController, not to form edits.
    // eslint-disable-next-line react-hooks/exhaustive-deps
  }, [])
  useEffect(() => {
    if (!pendingID && !uncertain) return
    const warn = (event: BeforeUnloadEvent) => { event.preventDefault() }
    window.addEventListener('beforeunload', warn)
    return () => window.removeEventListener('beforeunload', warn)
  }, [pendingID, uncertain])
  useEffect(() => {
    const abort = new AbortController(); let timer: ReturnType<typeof setTimeout> | undefined
    const poll = async () => {
      try {
        const result = await gatewayRequest<Job[]>(archived ? '/jobs?archived=1' : '/jobs', 'GET', undefined, abort.signal)
        if (!abort.signal.aborted) { setJobs(result); setJobsError('') }
      } catch (e) { if (!abort.signal.aborted) setJobsError(errorText(e)) }
      if (!abort.signal.aborted && !archived) timer = setTimeout(poll, 5000)
    }
    void poll()
    return () => { abort.abort(); if (timer) clearTimeout(timer) }
  }, [archived, reload])
  useEffect(() => {
    if (!settings?.auto_recovery_supported) return
    const abort = new AbortController(); let timer: ReturnType<typeof setTimeout> | undefined
    const poll = async () => {
      try {
        const result = await gatewayRequest<Recovery[]>('/recovery', 'GET', undefined, abort.signal)
        if (!abort.signal.aborted) { setRecoveries(result); setRecoveryError('') }
      } catch (e) { if (!abort.signal.aborted) setRecoveryError(errorText(e)) }
      if (!abort.signal.aborted) timer = setTimeout(poll, 5000)
    }
    void poll()
    return () => { abort.abort(); if (timer) clearTimeout(timer) }
  }, [settings?.auto_recovery_supported, reload])

  const setOption = (key: string, value: string) => { setOpts(old => ({ ...old, [key]: value })); invalidate(); setProxyResult('') }
  const inputProblem = () => {
    if (uncertain) return tr('Resolve the original submission below before creating another job.', '先核查下方原请求，结果明确前不能创建新任务。')
    if (!settings) return tr('Load gateway settings first.', '请先读取网关设置；读取失败时使用上方重试。')
    if (!settings.configured) return tr('Save the gateway URL and customer CDK first.', '请先保存网关地址和客户 CDK。')
    if (settingsDirty) return tr('Connection settings changed. Save or restore them first.', '连接设置尚未保存，请先保存或还原输入。')
    if (!caps) return tr('Capabilities are not loaded. Retry above.', '服务能力尚未读取，请在上方重试读取能力。')
    if (!source.trim()) return tr('Paste account material or choose a UTF-8 file.', '请粘贴账号材料，或选择 UTF-8 文件。')
    let count: number
    try { count = parseMaterial(source, format).length } catch (e) { return tr('Fix the material format: ', '请修正材料格式：') + errorText(e) }
    if (repairing && (!/^[1-9]\d*$/.test(recoveryAccountID) || !Number.isSafeInteger(Number(recoveryAccountID)))) return tr('Use a positive integer account ID.', '原账号 ID 必须是正整数。')
    if (repairing && count !== 1) return tr('Repairing an existing account requires exactly one account.', '修复已有账号时只能提供一个账号的材料。')
    if ((saveCredentials || repairing) && !settings.auto_recovery_supported) return tr('This server does not support controlled account recovery.', '当前服务不支持受控账号恢复，请先升级服务端。')
    if ((repairing || saveCredentials) && !caps.features?.expected_identity_guard) return tr('The gateway cannot verify the original identity; repair and automatic recovery are unavailable.', '网关未提供原身份校验能力，不能修复原账号或启用自动恢复。')
    if (opts.proxy_mode === 'custom' && !opts.proxy) return tr('Enter the OAuth proxy URI.', '请输入自带 OAuth 代理 URI，或改用系统代理。')
    if (opts.user_agent && opts.environment !== 'custom_ua') return tr('Clear User-Agent or choose custom_ua.', '请清空 User-Agent，或将环境改为 custom_ua。')
    for (const [key, values] of [['country', caps.countries], ['environment', caps.environments], ['workspace_policy', caps.workspace_policies], ['proxy_mode', caps.proxy_modes]] as const) {
      if (opts[key] && values && !values.includes(opts[key])) return tr(`The selected ${key} is no longer supported. Choose another value.`, `已选 ${key} 不再受网关支持，请重新选择。`)
    }
    if (!repairing && invalidGroups.length) return tr('Remove unavailable destination groups below.', '所选目标分组已失效，请在分组区域清除失效选择。')
    if (quota?.disabled) return tr('The gateway CDK is disabled. Check the connection credentials.', '网关 CDK 已停用，请检查连接凭证。')
    if (quota?.remaining === 0) return tr('No authorization quota remains. Refill and refresh quota.', '授权配额已用完，请补充配额后刷新。')
    return ''
  }
  const blocker = inputProblem()
  const previewReady = !!preview?.result.valid && previewMatches(preview, revision)
  const operationReason = pending.submit ? tr('Submitting; do not repeat.', '正在提交，请勿重复操作。') : pending.preview ? tr('Checking the current material…', '正在预检当前材料…') : setupBusy ? tr('Reading or updating connection data…', '正在读取或更新连接信息…') : ''
  const previewReason = operationReason || blocker
  const submitReason = previewReason || (previewChanged ? tr('Material or options changed. Run preview again.', '材料或选项已修改，预检已失效，请重新预检。') : !previewReady ? tr('Run preview and resolve any errors before creating a job.', '先点击预检并通过校验，再创建授权任务。') : '')
  const makeInput = (): GatewayInput => {
    const problem = inputProblem()
    if (problem) throw new Error(problem)
    const accounts = parseMaterial(source, format)
    const options = Object.fromEntries(Object.entries(opts).filter(([key, value]) => value && (key !== 'proxy' || opts.proxy_mode === 'custom')))
    return { ...(format === 'json' ? { accounts } : { source_text: source.replace(/^\uFEFF/, '') }), options, group_ids: repairing ? [] : selectedGroups, save_credentials: saveCredentials, ...(repairing ? { recovery_account_id: Number(recoveryAccountID) } : runtimeProxy ? { runtime_proxy: runtimeProxy } : {}) }
  }
  const saveSettings = () => run('save', async signal => {
    invalidate(); setConnection('')
    await gatewayRequest('/settings', 'PUT', { oauth_url: url.trim(), oauth_token: token }, signal)
    const data = await gatewayRequest<Settings>('/settings', 'GET', undefined, signal)
    if (signal.aborted) return
    setToken(''); setUrl(data.oauth_url); setSettings(data); setCaps(null); capsRef.current = null; setQuota(null)
    setMessage(tr('Settings saved. Connection data is refreshing.', '设置已保存，正在重新读取连接信息。'))
    await loadSetup()
  })
  const testConnection = () => run('test', async signal => {
    const data = await gatewayRequest<Connection>('/connections/test', 'POST', {}, signal)
    if (signal.aborted) return
    setConnection(`OAuth: ${data.oauth.ok ? 'OK' : problemText(data.oauth.error)}; ${tr('Local groups', '本地分组')}: ${data.codex.ok ? 'OK' : problemText(data.codex.error)}`)
    if (data.oauth.ok) {
      if (data.oauth.capabilities) { applyCapabilities(data.oauth.capabilities); setErrors(old => ({ ...old, capabilities: '' })) }
      else await loadCapabilities()
      if (data.oauth.quota) { setQuota(data.oauth.quota); setErrors(old => ({ ...old, quota: '' })) }
      else await loadQuota()
    }
    if (data.codex.ok) {
      if (data.codex.groups) { setGroups(data.codex.groups); setErrors(old => ({ ...old, groups: '' })) }
      else await loadGroups()
    }
  })
  const checkPreview = () => run('preview', async signal => {
    const input = makeInput(); const rev = revisionRef.current
    setPreview(null); setErrors(old => ({ ...old, submit: '', material: '' }))
    const result = await gatewayRequest<Preview>('/preview', 'POST', input, signal)
    if (!signal.aborted && rev === revisionRef.current) { hadPreview.current = true; setPreviewChanged(false); setPreview({ revision: rev, input, result }) }
  })
  const submit = () => run('submit', async signal => {
    if (!preview || !previewMatches(preview, revisionRef.current) || !preview.result.valid || uncertain || blocker) return
    if (!globalThis.crypto?.randomUUID) throw new Error(tr('Use HTTPS to generate a safe request ID.', '请使用 HTTPS 打开后台，以生成安全的请求 ID。'))
    const payload = createSubmission(preview.input, crypto.randomUUID())
    // If browser storage is unavailable, stop before sending the non-idempotent request.
    rememberPendingRequest(payload.request_id!)
    setPendingID(payload.request_id!)
    try {
      const job = await gatewayRequest<Job>('/jobs', 'POST', payload, signal)
      if (signal.aborted) return
      if (!job || job.id !== payload.request_id) throw new GatewayError(200, { message: 'Unexpected job response' }, true)
      forgetPendingRequest(); clearMaterial()
      setMessage(tr(`Job accepted: ${job.id}`, `任务已接受：${job.id}`)); setArchived(false); setReload(n => n + 1)
    } catch (e) {
      if (signal.aborted) return
      if (e instanceof GatewayError && e.uncertain) { setUncertain(payload.request_id!); clearMaterial() }
      else { forgetPendingRequest(); invalidate(); throw e }
    } finally { if (!signal.aborted) setPendingID('') }
  })
  const reconcile = () => run('reconcile', async signal => {
    if (!uncertain) return
    const job = await gatewayRequest<Job>(`/jobs/${encodeURIComponent(uncertain)}`, 'GET', undefined, signal)
    if (signal.aborted) return
    if (!job || job.id !== uncertain) throw new Error(tr('The response did not match the original request.', '返回结果与原请求不匹配，请管理员核查。'))
    forgetPendingRequest(); setUncertain(null); clearMaterial()
    setMessage(tr(`Original job found: ${job.id}`, `已找到原任务：${job.id}`)); setArchived(false); setReload(n => n + 1)
  })
  const action = (job: Job, path: string) => run(`job:${job.id}`, async signal => {
    if (path === 'archive' && !window.confirm(job.retained_material ? tr('Discard remaining task credentials and archive? Separately enrolled recovery is unchanged.', '清除该任务剩余凭据并归档？已单独启用的自动恢复不受影响。') : tr('Archive this task? Only statistics and deduplication records remain.', '归档此任务？仅保留统计和去重记录。'))) return
    await gatewayRequest(`/jobs/${encodeURIComponent(job.id)}/${path}`, 'POST', path === 'archive' ? { discard_credentials: job.retained_material } : {}, signal)
    if (!signal.aborted) setReload(n => n + 1)
  })
  const disableRecovery = (entry: Recovery) => run(`recovery:${entry.account_id}`, async signal => {
    if (!window.confirm(tr(`Stop automatic recovery for account #${entry.account_id} and discard its saved login material?`, `停止账号 #${entry.account_id} 的自动恢复并清除保管的登录材料？`))) return
    await gatewayRequest(`/recovery/${entry.account_id}/disable`, 'POST', {}, signal)
    if (!signal.aborted) setReload(n => n + 1)
  })
  const select = (name: string, key: string, values?: string[], fallback = '') => {
    const options = [{ value: fallback, label: fallback === 'system' ? tr('System proxy', '系统代理') : key === 'workspace_policy' ? tr('Team preferred (default)', '团队优先（默认）') : tr('Automatic (default)', '自动（默认）') }, ...(values || []).filter(v => v !== fallback).map(value => ({ value, label: value }))]
    if (opts[key] && !options.some(v => v.value === opts[key])) options.push({ value: opts[key], label: `${opts[key]} (${tr('unavailable', '已失效')})` })
    return label(name, <Select aria-label={name} value={opts[key] || fallback} disabled={!caps || formLocked} onValueChange={value => setOption(key, value)} options={options} />)
  }
  const showError = (key: string) => errors[key] ? <p role="alert" className="break-words text-sm text-destructive">{errors[key]}</p> : null

  return <div className="space-y-6">
    <PageHeader title={tr('OAuth gateway', 'OAuth 网关')} description={tr('Load connection data, check account material, then authorize. Job results and recovery stay visible below.', '读取连接信息、预检账号材料后再授权。任务结果和自动恢复状态都在下方查看。')} />
    <ol aria-label={tr('Authorization progress', '授权进度')} className="grid gap-3 border-b border-border pb-4 text-sm sm:grid-cols-3">
      <li><span className="font-medium">1. {tr('Connection', '连接就绪')}</span><p className="mt-1 text-muted-foreground">{settings?.configured && caps ? tr('Ready', '已就绪') : tr('Save settings and load capabilities', '保存设置并读取服务能力')}</p></li>
      <li><span className="font-medium">2. {tr('Preview', '材料预检')}</span><p className="mt-1 text-muted-foreground">{previewReady ? tr('Passed; ready to submit', '已通过，可以提交') : previewChanged ? tr('Changed; preview again', '已修改，需要重新预检') : tr('Fill material, then preview', '填写材料后点击预检')}</p></li>
      <li><span className="font-medium">3. {tr('Authorization and import', '授权与导入')}</span><p className="mt-1 text-muted-foreground">{tr('Create once; follow the original job', '只提交一次，持续查看原任务')}</p></li>
    </ol>
    {uncertain && <div className="space-y-3 rounded-md border border-amber-500/40 bg-amber-500/10 p-4 text-sm" role="status">
      <p className="font-medium">{tr('Submission needs reconciliation', '上次提交结果待核查，已暂停创建新任务')}</p>
      <p>{tr('Uncertain request ID', '结果不确定的请求 ID')}: <span className="break-all">{uncertain}</span></p>
      <p>{tr('Only this request ID is kept in the browser session. Query it first; not found does not prove the authorization never started. If it remains missing, give this ID to the administrator instead of submitting again.', '浏览器只保留此请求 ID，不保存登录材料。先查询原任务；“未找到”不等于从未执行。持续查不到时，请管理员按此 ID 对账，不要重新提交。')}</p>
      <Button variant="outline" disabled={!!pending.reconcile} onClick={reconcile}>{pending.reconcile ? tr('Looking up…', '正在查找…') : tr('Find original job', '查找原任务')}</Button>{showError('reconcile')}
    </div>}
    {message && <p role="status" className="break-words rounded-md border border-border p-3 text-sm">{message}</p>}
    <Card><CardHeader><CardTitle>{tr('Gateway connection', '网关连接')}</CardTitle></CardHeader><CardContent className="space-y-4">
      <div className="grid gap-4 md:grid-cols-2">
        {label(tr('OAuth URL', 'OAuth 地址'), <Input value={url} disabled={configLocked} onChange={e => setUrl(e.target.value)} autoComplete="off" />, tr('Use the gateway root URL, without /docs.', '填写网关根地址，不要带 /docs。'))}
        {label(tr('Customer CDK', '客户 CDK（激活码）'), <Input type="password" value={token} disabled={configLocked} onChange={e => setToken(e.target.value)} autoComplete="new-password" />, settings?.oauth_token_set ? tr('Set; leave blank to keep it. This is not your codex2api admin key.', '已设置；留空保留。这不是 codex2api 管理密钥。') : tr('Use the OAuth gateway customer activation code.', '填写 OAuth 网关提供的客户激活码。'))}
      </div>
      <div className="flex flex-wrap gap-2">
        <Button disabled={configLocked} onClick={saveSettings}>{pending.save ? tr('Saving…', '正在保存…') : tr('Save settings', '保存设置')}</Button>
        {settingsDirty && <Button variant="outline" disabled={configLocked} onClick={() => { setUrl(settings!.oauth_url); setToken('') }}>{tr('Restore input', '还原输入')}</Button>}
        <Button variant="outline" disabled={configLocked || !settings?.configured || settingsDirty} onClick={testConnection}>{pending.test ? tr('Testing…', '正在测试…') : tr('Test connections', '测试连接')}</Button>
        <Button variant="outline" disabled={configLocked || !settings?.configured || settingsDirty} onClick={() => void loadSetup()}>{setupBusy && !pending.save && !pending.test ? tr('Reading…', '正在读取…') : tr('Load capabilities and quota', '读取能力和配额')}</Button>
      </div>
      <p className="text-sm text-muted-foreground">{!settings ? tr('Reading settings; retry below if it fails.', '正在读取设置；失败时请使用下方重试。') : !settings.configured ? tr('Not configured: save your URL and CDK first.', '尚未配置：先保存地址和 CDK。') : settingsDirty ? tr('Unsaved connection changes; save or restore them first.', '连接设置有未保存的修改，请先保存或还原。') : formLocked ? tr('Submission pending; connection changes are temporarily locked.', '提交结果尚未明确，暂不能修改连接。') : pending.preview ? tr('Preview in progress; connection changes are temporarily locked.', '正在预检，暂不能修改连接。') : setupBusy ? tr('Reading or updating connection data…', '正在读取或更新连接信息…') : tr('Configured. Connection data loads automatically; refresh preserves your choices.', '已配置。进入页面会自动读取连接信息，刷新不会清空选择。')}</p>
      {showError('settings')}{errors.settings && <Button variant="outline" disabled={!!pending.settings} onClick={loadSettings}>{tr('Retry settings', '重试读取设置')}</Button>}{showError('save')}{showError('test')}
      {connection && <p role="status" className="break-words text-sm">{connection}</p>}
      <div className="grid gap-3 border-t border-border pt-4 text-sm sm:grid-cols-2">
        <div className="space-y-2"><p className="font-medium">{tr('Service capabilities', '服务能力')}</p><p className="text-muted-foreground">{caps ? tr(`Batch limit ${caps.max_batch_size ?? '—'}; upload limit ${caps.max_upload_bytes ?? '—'} bytes.`, `批量上限 ${caps.max_batch_size ?? '—'}；上传上限 ${caps.max_upload_bytes ?? '—'} 字节。`) : pending.capabilities ? tr('Loading…', '正在读取…') : tr('Not loaded yet.', '尚未读取。')}</p>{showError('capabilities')}{errors.capabilities && <Button size="sm" variant="outline" disabled={configLocked || settingsDirty} onClick={loadCapabilities}>{tr('Retry capabilities', '重试读取能力')}</Button>}</div>
        <div className="space-y-2"><p className="font-medium">{tr('Quota remaining / total', '配额剩余 / 总计')}: {quota?.remaining ?? '—'} / {quota?.total ?? '—'}</p><p className="text-muted-foreground">{quota ? tr(`Reserved ${quota.reserved ?? '—'}; used ${quota.used ?? '—'}.`, `已预留 ${quota.reserved ?? '—'}；已使用 ${quota.used ?? '—'}。`) : pending.quota ? tr('Loading…', '正在读取…') : tr('Unknown; not treated as zero.', '尚未取得配额，不视为零。')}</p>{showError('quota')}{errors.quota && <><p className="text-xs text-muted-foreground">{tr('Other data remains available. Authorization will recheck quota.', '其他已读取数据仍可用，授权执行时会再次检查配额。')}</p><Button size="sm" variant="outline" disabled={configLocked || settingsDirty} onClick={loadQuota}>{tr('Retry quota', '重试读取配额')}</Button></>}</div>
      </div>
    </CardContent></Card>
    <Card><CardHeader><CardTitle>{tr('Prepare authorization', '准备授权')}</CardTitle></CardHeader><CardContent className="space-y-4">
      <fieldset disabled={formLocked} className="space-y-4">
        <p className="max-w-4xl text-sm text-muted-foreground">{tr('TXT: identifier----password----TOTP_SECRET, one per line. Use a JSON array for per-account proxies or passwords containing ----. Maximum 1 MiB / 100 accounts. Material is shown in full; the browser does not persist it and clears it after acceptance. Server tasks are encrypted.', 'TXT 每行：账号----密码----TOTP_SECRET。单账号代理或密码包含 ---- 时请用 JSON 数组。最多 1 MiB、100 个账号。材料完整显示，浏览器不持久保存，接受任务后清空；服务端任务加密存储。')}</p>
        <div className="grid gap-4 md:grid-cols-2">
          {label(tr('Input format', '输入格式'), <Select aria-label={tr('Input format', '输入格式')} value={format} disabled={formLocked} onValueChange={v => { setFormat(v as 'txt' | 'json'); invalidate() }} options={[{ value: 'txt', label: 'TXT' }, { value: 'json', label: 'JSON' }]} />)}
          {label(tr('UTF-8 file', 'UTF-8 文件'), <input ref={fileRef} type="file" accept=".txt,.json,text/plain,application/json" className={fieldClass} onChange={e => {
            const file = e.target.files?.[0]; if (!file) return
            setSource(''); invalidate(); setErrors(old => ({ ...old, material: '' }))
            const rev = revisionRef.current; const owner = controller.current
            if (file.size > 1048576) { setErrors(old => ({ ...old, material: tr('File exceeds 1 MiB.', '文件超过 1 MiB。') })); return }
            void file.arrayBuffer().then(bytes => {
              if (owner && !owner.signal.aborted && rev === revisionRef.current) { setSource(new TextDecoder('utf-8', { fatal: true }).decode(bytes)); invalidate() }
            }).catch(() => { if (owner && !owner.signal.aborted && rev === revisionRef.current) setErrors(old => ({ ...old, material: tr('File must be valid UTF-8.', '文件必须是有效 UTF-8。') })) })
          }} />)}
        </div>
        {label(tr('Account material', '账号材料'), <textarea aria-label={tr('Account material', '账号材料')} className={`${fieldClass} min-h-32 font-mono`} value={source} autoComplete="off" autoCorrect="off" spellCheck={false} onChange={e => { setSource(e.target.value); setErrors(old => ({ ...old, material: '' })); invalidate() }} />)}{showError('material')}
        <div className="grid gap-4 md:grid-cols-2 xl:grid-cols-4">{select(tr('Country', '国家'), 'country', caps?.countries)}{select(tr('Environment', '环境'), 'environment', caps?.environments)}{select(tr('Workspace policy', '工作区策略'), 'workspace_policy', caps?.workspace_policies)}{select(tr('OAuth proxy mode', 'OAuth 代理模式'), 'proxy_mode', caps?.proxy_modes, 'system')}</div>
        {!caps && <p className="text-sm text-muted-foreground">{tr('Options become available after capabilities load. Material can be entered now.', '服务能力读取成功后即可选择选项；现在可以先填写材料。')}</p>}
        {opts.proxy_mode === 'custom' && <div className="space-y-2">
          {label(tr('OAuth proxy URI', 'OAuth 代理 URI'), <Input type="password" value={opts.proxy || ''} autoComplete="new-password" onChange={e => setOption('proxy', e.target.value)} />)}
          <Button variant="outline" disabled={!!pending.proxy || !caps?.features?.proxy_check || !opts.proxy || settingsDirty || setupBusy} onClick={() => void run('proxy', async signal => {
            const options = { ...opts }; delete options.workspace_policy; const rev = revisionRef.current
            const result = await gatewayRequest<ProxyCheck>('/proxy-check', 'POST', { options }, signal)
            if (!signal.aborted && rev === revisionRef.current) setProxyResult(`${result.usable ? tr('Usable', '可用') : tr('Not usable', '不可用')} · ${tr('Requested / observed country', '请求 / 实际地区')}: ${result.requested_country || '—'} / ${result.observed_country || '—'} · OpenAI: ${result.openai_reachable ? 'OK' : '—'} · HTTP ${result.openai_http_status ?? '—'} · Trace ${result.trace_http_status ?? '—'} ${problemText(result.error)}`)
          })}>{pending.proxy ? tr('Checking proxy…', '正在检查代理…') : tr('Check OAuth proxy from execution machine', '从执行机器检查 OAuth 代理')}</Button>
          {!caps?.features?.proxy_check ? <p className="text-xs text-muted-foreground">{tr('The gateway has not declared proxy-check support.', '网关未声明支持执行机代理检查。')}</p> : !opts.proxy ? <p className="text-xs text-muted-foreground">{tr('Enter a proxy URI before checking.', '填写 OAuth 代理 URI 后即可检查。')}</p> : null}{showError('proxy')}{proxyResult && <p role="status" className="break-words text-sm">{proxyResult}</p>}
        </div>}
        <div className="grid gap-4 md:grid-cols-2">
          {label(tr('Runtime proxy URI (optional)', '运行时代理 URI（可选）'), <Input type="password" value={runtimeProxy} disabled={repairing} autoComplete="new-password" onChange={e => { setRuntimeProxy(e.target.value); invalidate() }} />, repairing ? tr('Repair preserves the original account proxy.', '修复已有账号时保留原账号代理，不使用此处输入。') : tr('For later model requests, not OAuth. Must already be enabled in proxy management; blank inherits normal policy.', '用于后续模型请求，不是 OAuth 执行代理。须先在代理管理启用；留空沿用正常策略。'))}
          {label('User-Agent', <Input value={opts.user_agent || ''} disabled={opts.environment !== 'custom_ua' && !opts.user_agent} onChange={e => setOption('user_agent', e.target.value)} />, tr('Only custom_ua allows this field. Otherwise leave it blank.', '仅 custom_ua 环境需要填写，其他环境请留空。'))}
          {label(tr('Language (optional)', '语言（可选）'), <Input value={opts.language || ''} onChange={e => setOption('language', e.target.value)} />)}
          {label(tr('Timezone (optional)', '时区（可选）'), <Input value={opts.timezone || ''} onChange={e => setOption('timezone', e.target.value)} />)}
        </div>
        <fieldset disabled={repairing} className="space-y-2"><legend className="mb-2 text-sm font-medium">{tr('Destination groups (optional)', '目标分组（可选）')}</legend>
          <p className="text-xs text-muted-foreground">{repairing ? tr('Repair preserves the original groups; selections here are not applied.', '修复已有账号时保留原分组，不使用此处选择。') : tr('Codex groups only. Leave unselected to import without group bindings.', '只列出 Codex 分组。不选择则导入后不绑定分组。')}</p>
          <div className="flex flex-wrap gap-4 text-sm">{groups?.length ? groups.map(g => <label key={g.id} className="flex items-center gap-2"><input type="checkbox" checked={selectedGroups.includes(g.id)} onChange={e => { setSelectedGroups(old => e.target.checked ? [...old, g.id] : old.filter(id => id !== g.id)); invalidate() }} />{g.name || `#${g.id}`}</label>) : <span className="text-muted-foreground">{pending.groups ? tr('Loading groups…', '正在读取分组…') : errors.groups ? tr('Groups could not be loaded; retry below.', '分组读取失败，请在下方重试。') : groups ? tr('No Codex groups exist. You can continue without a group.', '暂无 Codex 分组，可以不分组继续。') : tr('Groups have not been loaded yet.', '分组尚未读取。')}</span>}</div>
          {showError('groups')}{errors.groups && <Button variant="outline" size="sm" disabled={!!pending.groups} onClick={loadGroups}>{tr('Retry groups', '重试读取分组')}</Button>}
          {!!invalidGroups.length && <div className="space-y-2 text-sm"><p role="alert">{tr('Unavailable selections', '已失效的分组选择')}: {invalidGroups.map(id => `#${id}`).join(', ')}</p><Button size="sm" variant="outline" onClick={() => { setSelectedGroups(old => old.filter(id => !invalidGroups.includes(id))); invalidate() }}>{tr('Remove unavailable selections', '清除失效选择')}</Button></div>}
          <Link className="inline-block text-xs text-primary underline underline-offset-4" to="/accounts">{tr('Manage groups in Accounts', '前往账号管理查看或创建分组')}</Link>
        </fieldset>
        <div className="space-y-3 border-t border-border pt-4">
          {label(tr('Existing account ID to repair (optional)', '修复已有账号 ID（可选）'), <Input inputMode="numeric" value={recoveryAccountID} disabled={!settings?.auto_recovery_supported && !recoveryAccountID} placeholder={tr('Blank creates a new account', '留空表示新增账号')} onChange={e => { setRecoveryAccountID(e.target.value.trim()); invalidate() }} />, tr('Specify the original account ID and one account’s material. Identity, workspace and credential version must match. Deleted or disabled accounts are not revived.', '填写原账号 ID，并只提供一条材料。必须通过身份、工作区与凭据版本校验；不会复活已删除或禁用账号。'))}
          <label className="flex items-start gap-2 text-sm"><input type="checkbox" className="mt-1" checked={saveCredentials} disabled={!settings?.auto_recovery_supported && !saveCredentials} onChange={e => { setSaveCredentials(e.target.checked); invalidate() }} /><span>{tr('Enable 401 recovery after success (encrypt and retain password and TOTP)', '成功后启用 401 自动恢复（加密保管密码和 TOTP）')}</span></label>
          <p className="text-xs text-muted-foreground">{settings?.auto_recovery_supported ? tr('Off by default. Only explicitly enrolled accounts may recover. Turn it off below to discard saved login material; archiving a job does not disable recovery.', '默认关闭，仅对明确启用的账号生效。可在下方停止恢复并清除保管材料；归档任务不会关闭已启用的恢复。') : settings?.auto_recovery_reason || tr('Waiting for server recovery capabilities.', '正在读取服务端恢复能力。')}</p>
        </div>
      </fieldset>
      <div className="space-y-3 border-t border-border pt-4">
        <p id="oauth-submit-reason" role="status" className="break-words text-sm">{submitReason || tr('Preview passed. Create one job to begin authorization.', '预检通过，可以创建授权任务。')}</p>
        <div className="flex flex-wrap gap-2"><Button variant="outline" disabled={!!previewReason} aria-describedby="oauth-submit-reason" onClick={checkPreview}>{pending.preview ? tr('Checking…', '预检中…') : tr('Preview', '预检')}</Button><Button disabled={!!submitReason} aria-describedby="oauth-submit-reason" onClick={submit}>{pending.submit ? tr('Submitting…', '提交中…') : repairing ? tr('Authorize and repair this account', '授权并修复原账号') : tr('Create authorization job', '创建授权任务')}</Button></div>
        {showError('preview')}{showError('submit')}
        {pendingID && <p className="break-all text-sm" role="status">{tr('Submitting request', '正在提交请求')}: {pendingID}</p>}
        {preview && <div className="space-y-1 rounded-md border border-border p-3 text-sm" role="status"><p>{preview.result.valid ? tr('Preview passed', '预检通过') : tr('Preview failed', '预检未通过')} · {preview.result.count} {tr('accounts', '个账号')} · {preview.result.groups} {tr('batches', '个批次')}</p>{preview.result.rows?.map((row, i) => <p className="break-all" key={i}>{row.line}. {identifier(row)} · {row.proxy_mode}</p>)}{preview.result.errors?.map((row, i) => <p key={i}>{tr('Line', '第')} {row.line} {tr('', '行')}: {row.code}</p>)}</div>}
      </div>
    </CardContent></Card>
    <Card><CardHeader><div className="flex flex-wrap items-center justify-between gap-3"><CardTitle>{tr('Authorization jobs', '授权任务')}</CardTitle><div className="flex gap-2"><Button variant="outline" size="sm" onClick={() => setReload(n => n + 1)}>{tr('Refresh list', '刷新列表')}</Button><Button variant="outline" size="sm" aria-pressed={archived} onClick={() => { setArchived(v => !v); setJobs(null) }}>{archived ? tr('Active jobs', '当前任务') : tr('Archived jobs', '归档任务')}</Button></div></div></CardHeader><CardContent className="space-y-3">
      <p className="text-xs text-muted-foreground">{tr('The list refreshes automatically. Refresh original continues an existing authorization; retry import only retries a confirmed local write failure, never OAuth.', '列表自动更新。“查询原任务”继续核查已有授权；“重试导入”只重试确定失败的入库，不重新授权。')}</p>
      {jobsError && <p role="alert" className="break-words text-sm text-destructive">{tr('Could not load jobs; use Refresh list:', '读取任务失败，请刷新列表：')} {jobsError}</p>}
      {jobs === null ? <p className="text-sm text-muted-foreground">{jobsError ? tr('Waiting for a successful retry.', '等待重试成功。') : tr('Loading jobs…', '正在读取任务…')}</p> : jobs.length === 0 ? <p className="text-sm text-muted-foreground">{tr('No jobs in this view.', '此处暂无任务。')}</p> : jobs.map(job => <article key={job.id} id={`oauth-job-${job.id}`} className="space-y-3 rounded-md border border-border p-4">
        <div className="flex flex-wrap items-start justify-between gap-2"><div className="min-w-0"><p className="break-all text-sm font-medium">{job.id}</p><p className="text-sm text-muted-foreground">{stateText(job.state)}{job.archived ? ` · ${tr('archived', '已归档')}` : ''}{job.recovery_account_id ? ` · ${tr('Original account', '原账号')} #${job.recovery_account_id}` : ''}</p></div><p className="text-sm">{tr('Imported', '已导入')} {job.imported}/{job.total} · {tr('Failed', '失败')} {job.failed} · {tr('Pending', '待处理')} {job.pending ?? '—'}</p></div>
        {!job.archived && <>
          <div className="flex flex-wrap gap-x-4 gap-y-3">{([
            ['refresh', job.can_refresh, tr('Refresh original', '查询原任务'), tr('The task has ended; no query is needed.', '任务已结束，无需继续查询。')],
            ['cancel', job.can_cancel, tr('Cancel', '取消'), tr('No cancellable items, or cancellation is already requested.', '没有可取消的项目，或已请求取消。')],
            ['retry-import', job.can_retry_import, tr('Retry import', '重试导入'), tr('No confirmed, retryable import failures.', '没有确定失败且可重试的入库项。')],
            ['archive', job.can_archive, tr('Archive', '归档'), tr('Resolve pending results or retryable imports first.', '先处理待核查结果或可重试入库项。')],
          ] as const).map(([path, allowed, text, reason]) => <div key={path} className="max-w-52 space-y-1"><Button variant="outline" size="sm" disabled={!allowed || !!pending[`job:${job.id}`]} aria-describedby={`reason-${job.id}-${path}`} onClick={() => action(job, path)}>{text}</Button><p id={`reason-${job.id}-${path}`} className="text-xs text-muted-foreground">{pending[`job:${job.id}`] ? tr('Processing this task…', '正在处理此任务…') : !allowed ? reason : path === 'archive' ? tr('Resolved; can archive.', '结果已明确，可以归档。') : ''}</p></div>)}</div>
          {showError(`job:${job.id}`)}
          <details><summary className="cursor-pointer text-sm font-medium focus-visible:outline-2 focus-visible:outline-ring">{tr('Batch and account details', '批次与账号详情')}</summary><div className="mt-3 space-y-3">{job.groups?.length ? job.groups.map(g => <div key={g.index} className="space-y-1 border-t border-border pt-3 text-sm"><p>{tr('Batch', '批次')} {g.index + 1}: {stateText(g.state)} · {g.imported}/{g.total}</p><p className="break-all text-muted-foreground">{tr('Batch ID', '批次 ID')}: {g.batch_id || '—'} · {tr('Request', '请求')}: {g.request_id || '—'}</p>{g.error && <p className="break-words text-destructive">{problemText(g.error)}</p>}{(g.read_failures || g.next_check_at) && <p>{tr('Read / failures / next check', '读取 / 失败次数 / 下次检查')}: {g.read_operation || '—'} / {g.read_failures || 0} / {g.next_check_at || '—'}</p>}{g.items?.map((item, i) => <div key={i} className="break-words pl-3">{item.line}. <span className="break-all">{identifier(item)}</span> · OAuth: {stateText(item.state)} · {tr('Import', '导入')}: {stateText(item.import_state)}{item.error && <p className="text-destructive">{problemText(item.error)}</p>}</div>)}</div>) : <p className="text-sm text-muted-foreground">{tr('No batch details yet.', '暂无批次详情。')}</p>}</div></details>
        </>}
      </article>)}
    </CardContent></Card>
    {settings?.auto_recovery_supported && <Card><CardHeader><div className="flex flex-wrap items-center justify-between gap-3"><CardTitle>{tr('Automatic account recovery', '账号自动恢复')}</CardTitle><Button size="sm" variant="outline" onClick={() => setReload(n => n + 1)}>{tr('Refresh recovery status', '刷新恢复状态')}</Button></div></CardHeader><CardContent className="space-y-3">
      <p className="text-sm text-muted-foreground">{tr('Only accounts explicitly enrolled above are listed. Recovery keeps the original ID, proxy and groups; identity changes or unknown results require review.', '这里只列出明确启用的账号。恢复保留原 ID、代理和分组；身份变化或结果未知时必须核查。')}</p>
      {recoveryError && <p role="alert" className="break-words text-sm text-destructive">{recoveryError}</p>}
      {recoveries === null ? <p className="text-sm text-muted-foreground">{recoveryError ? tr('Retry with Refresh recovery status.', '请点击刷新恢复状态重试。') : tr('Loading recovery status…', '正在读取恢复状态…')}</p> : !recoveries.length ? <p className="text-sm text-muted-foreground">{tr('No enrolled accounts. Opt in above when authorizing a new account or repairing an existing one.', '暂无启用的账号。在上方新增或修复账号时，明确勾选加密保管材料即可启用。')}</p> : recoveries.map(entry => <div key={entry.account_id} className="space-y-2 border-t border-border pt-3 text-sm"><div className="flex flex-wrap items-center justify-between gap-3"><p className="min-w-0 break-all">#{entry.account_id} {entry.identifier}</p><Button variant="outline" size="sm" disabled={!entry.can_disable || !!pending[`recovery:${entry.account_id}`]} onClick={() => disableRecovery(entry)}>{pending[`recovery:${entry.account_id}`] ? tr('Stopping…', '正在停止…') : tr('Stop recovery and clear material', '停止恢复并清除材料')}</Button></div><p>{stateText(entry.state)} · {tr('Attempts', '尝试次数')}: {entry.attempts}{entry.next_attempt_at ? ` · ${tr('Next attempt', '下次检查')}: ${entry.next_attempt_at}` : ''}</p>{!entry.can_disable && <p className="text-xs text-muted-foreground">{tr('Recovery is not enabled for this entry.', '此条目未启用恢复，无需再次停止。')}</p>}{entry.job_id && <p className="break-all">{tr('Original job', '关联任务')}: {entry.job_id}</p>}{entry.error && <p className="break-words text-destructive">{problemText(entry.error)}</p>}{showError(`recovery:${entry.account_id}`)}</div>)}
    </CardContent></Card>}
  </div>
}
