import { useEffect, useRef, useState, type ReactNode } from 'react'
import { useTranslation } from 'react-i18next'
import PageHeader from '../components/PageHeader'
import { Button } from '../components/ui/button'
import { Card, CardContent, CardHeader, CardTitle } from '../components/ui/card'
import { Input } from '../components/ui/input'
import { Select } from '../components/ui/select'
import { createSubmission, parseMaterial, previewMatches, readPendingRequest, rememberPendingRequest, forgetPendingRequest, type GatewayInput, type Problem } from '../lib/oauthGateway'
import { gatewayRequest, GatewayError } from '../lib/oauthGatewayApi'

type Settings = { oauth_url: string; oauth_token_set: boolean; configured: boolean; auto_recovery_supported: false; auto_recovery_reason: string }
type Capabilities = { countries?: string[]; environments?: string[]; proxy_modes?: string[]; workspace_policies?: string[]; max_batch_size?: number; max_upload_bytes?: number; features?: Record<string, boolean> }
type Quota = { remaining?: number; total?: number; reserved?: number; used?: number; disabled?: boolean }
type Group = { id: number; name: string }
type Preview = { valid: boolean; count: number; groups: number; rows?: { line: number; identifier_masked: string; proxy_mode: string }[]; errors?: { line: number; code: string }[] }
type Job = { id: string; state: string; total: number; imported: number; failed: number; pending: number; can_refresh: boolean; can_cancel: boolean; can_retry_import: boolean; can_archive: boolean; retained_material: boolean; archived: boolean; groups?: { index: number; state: string; batch_id: string; request_id: string; imported: number; total: number; error?: Problem; read_operation?: string; read_failures?: number; next_check_at?: string; items?: { line: number; identifier_masked: string; state: string; import_state: string; error?: Problem }[] }[] }
type Connection = { oauth: { ok: boolean; error?: Problem }; codex: { ok: boolean; error?: Problem } }
type ProxyCheck = { usable: boolean; requested_country?: string; observed_country?: string; openai_reachable?: boolean; openai_http_status?: number; trace_http_status?: number; error?: Problem }
const fieldClass = 'w-full rounded-md border border-input bg-transparent px-3 py-2 text-sm outline-none focus-visible:ring-2 focus-visible:ring-ring'
const problemText = (p?: Problem) => p ? [p.code, p.message, p.action, p.source, p.retry_after != null ? `retry after ${p.retry_after}s` : ''].filter(Boolean).join(' · ') : ''
const errorText = (e: unknown) => e instanceof Error ? e.message : 'Request failed'
const label = (name: string, child: ReactNode, hint?: string) => <label className="flex min-w-0 flex-col gap-2 text-sm"><span className="font-medium">{name}</span>{child}{hint && <span className="text-xs text-muted-foreground">{hint}</span>}</label>

export default function OAuthGateway() {
  const { i18n } = useTranslation()
  const zh = i18n.resolvedLanguage?.startsWith('zh') ?? false
  const tr = (en: string, cn: string) => zh ? cn : en
  const [settings, setSettings] = useState<Settings | null>(null)
  const [url, setUrl] = useState('https://oauth.cala.lol')
  const [token, setToken] = useState('')
  const [caps, setCaps] = useState<Capabilities | null>(null)
  const [quota, setQuota] = useState<Quota | null>(null)
  const [groups, setGroups] = useState<Group[]>([])
  const [selectedGroups, setSelectedGroups] = useState<number[]>([])
  const [source, setSource] = useState('')
  const [format, setFormat] = useState<'txt' | 'json'>('txt')
  const [opts, setOpts] = useState<Record<string, string>>({ proxy_mode: 'system' })
  const [runtimeProxy, setRuntimeProxy] = useState('')
  const [preview, setPreview] = useState<{ revision: number; input: GatewayInput; result: Preview } | null>(null)
  const [revision, setRevision] = useState(0)
  const revisionRef = useRef(0)
  const [uncertain, setUncertain] = useState<string | null>(readPendingRequest)
  const [pendingID, setPendingID] = useState('')
  const [jobs, setJobs] = useState<Job[] | null>(null)
  const [jobsError, setJobsError] = useState('')
  const [archived, setArchived] = useState(false)
  const [reload, setReload] = useState(0)
  const [message, setMessage] = useState('')
  const [connection, setConnection] = useState('')
  const [proxyResult, setProxyResult] = useState('')
  const [busy, setBusy] = useState('')
  const busyRef = useRef(false)
  const controller = useRef<AbortController | null>(null)
  const fileRef = useRef<HTMLInputElement>(null)
  const maskedText = typeof CSS !== 'undefined' && CSS.supports('-webkit-text-security', 'disc')
  const invalidate = () => { revisionRef.current++; setRevision(revisionRef.current); setPreview(null) }
  const clearMaterial = () => { setSource(''); setRuntimeProxy(''); setOpts(current => ({ ...current, proxy: '' })); if (fileRef.current) fileRef.current.value = ''; invalidate() }
  const run = async (name: string, fn: (signal: AbortSignal) => Promise<void>) => {
    if (busyRef.current || !controller.current) return
    busyRef.current = true; setBusy(name); setMessage('')
    const signal = controller.current.signal
    try { await fn(signal) } catch (e) { if (!signal.aborted) setMessage(errorText(e)) } finally { busyRef.current = false; if (!signal.aborted) setBusy('') }
  }
  useEffect(() => {
    const abort = new AbortController(); controller.current = abort
    void gatewayRequest<Settings>('/settings', 'GET', undefined, abort.signal).then(s => { if (!abort.signal.aborted) { setSettings(s); setUrl(s.oauth_url || 'https://oauth.cala.lol') } }).catch(e => { if (!abort.signal.aborted) setMessage(errorText(e)) })
    return () => { abort.abort(); controller.current = null; revisionRef.current++; }
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
    setJobs(null); void poll()
    return () => { abort.abort(); if (timer) clearTimeout(timer) }
  }, [archived, reload])
  const setOption = (key: string, value: string) => { setOpts(old => ({ ...old, [key]: value })); invalidate() }
  const makeInput = (): GatewayInput => {
    if (!settings?.configured) throw new Error(tr('Configure the gateway first.', '请先配置网关。'))
    if (!caps) throw new Error(tr('Load capabilities first.', '请先读取服务能力。'))
    const accounts = parseMaterial(source, format)
    if (opts.proxy_mode === 'custom' && !opts.proxy) throw new Error(tr('Enter the OAuth proxy URI.', '请输入 OAuth 代理 URI。'))
    if (opts.user_agent && opts.environment !== 'custom_ua') throw new Error(tr('User-Agent requires custom_ua environment.', 'User-Agent 仅适用于 custom_ua 环境。'))
    const options = Object.fromEntries(Object.entries(opts).filter(([key, value]) => value && (key !== 'proxy' || opts.proxy_mode === 'custom')))
    return { ...(format === 'json' ? { accounts } : { source_text: source.replace(/^\uFEFF/, '') }), options, group_ids: selectedGroups, save_credentials: false, ...(runtimeProxy ? { runtime_proxy: runtimeProxy } : {}) }
  }
  const saveSettings = () => run('settings', async signal => {
    await gatewayRequest('/settings', 'PUT', { oauth_url: url, oauth_token: token }, signal)
    const data = await gatewayRequest<Settings>('/settings', 'GET', undefined, signal)
    if (signal.aborted) return
    setToken(''); setSettings(data); setCaps(null); invalidate(); setMessage(tr('Settings saved. Reload capabilities.', '设置已保存，请重新读取服务能力。'))
  })
  const loadCapabilities = () => run('capabilities', async signal => {
    invalidate(); setCaps(null)
    const [c, q, g] = await Promise.all([gatewayRequest<Capabilities>('/capabilities', 'GET', undefined, signal), gatewayRequest<Quota>('/quota', 'GET', undefined, signal), gatewayRequest<Group[]>('/groups', 'GET', undefined, signal)])
    if (signal.aborted) return
    setCaps(c); setQuota(q); setGroups(g || []); setSelectedGroups([]); setOpts({ proxy_mode: c.proxy_modes?.includes('system') ? 'system' : c.proxy_modes?.[0] || '' })
  })
  const submit = () => run('submit', async signal => {
    if (!preview || !previewMatches(preview, revisionRef.current) || uncertain) return
    if (!globalThis.crypto?.randomUUID) throw new Error(tr('A secure context is required to generate a request ID.', '需要安全上下文才能生成请求 ID。'))
    const payload = createSubmission(preview.input, crypto.randomUUID())
    rememberPendingRequest(payload.request_id!)
    setPendingID(payload.request_id || '')
    try {
      const job = await gatewayRequest<Job>('/jobs', 'POST', payload, signal)
      if (signal.aborted) return
      if (!job || job.id !== payload.request_id) throw new GatewayError(200, { message: 'Unexpected job response' }, true)
      forgetPendingRequest()
      clearMaterial(); setMessage(tr(`Job accepted: ${job.id}`, `任务已接受：${job.id}`)); setArchived(false); setReload(n => n + 1)
    } catch (e) {
      if (signal.aborted) return
      if (e instanceof GatewayError && e.uncertain) { setUncertain(payload.request_id!); clearMaterial(); setMessage(tr('Submission outcome is uncertain. Reconcile the original request; do not create another job.', '提交结果不确定。请核查原请求，不要创建新任务。')) }
      else { forgetPendingRequest(); invalidate(); throw e }
    } finally { if (!signal.aborted) setPendingID('') }
  })
  const reconcile = () => run('reconcile', async signal => {
    if (!uncertain) return
    const job = await gatewayRequest<Job>(`/jobs/${encodeURIComponent(uncertain)}`, 'GET', undefined, signal)
    if (signal.aborted) return
    if (!job || job.id !== uncertain) throw new Error(tr('The response did not match the original request.', '返回结果与原请求不匹配。'))
    forgetPendingRequest(); setUncertain(null); clearMaterial(); setMessage(tr(`Original job found: ${job.id}`, `已找到原任务：${job.id}`)); setArchived(false); setReload(n => n + 1)
  })
  const action = (job: Job, path: string) => run(`${job.id}:${path}`, async signal => {
    if (path === 'archive' && !window.confirm(job.retained_material ? tr('Archiving discards retained credentials and task material. Continue?', '归档将清除保留的凭据及任务材料。继续吗？') : tr('Archive this task? Only statistics and deduplication records remain.', '归档此任务？仅保留统计和去重记录。'))) return
    await gatewayRequest(`/jobs/${encodeURIComponent(job.id)}/${path}`, 'POST', path === 'archive' ? { discard_credentials: job.retained_material } : {}, signal)
    if (!signal.aborted) setReload(n => n + 1)
  })
  const select = (name: string, key: string, values?: string[], defaultValue = '') => label(name, <Select aria-label={name} value={opts[key] || defaultValue} disabled={!caps || !!uncertain} onValueChange={value => setOption(key, value)} options={[{ value: defaultValue, label: defaultValue === 'system' ? tr('System proxy', '系统代理') : tr('Service default', '服务默认') }, ...(values || []).filter(v => v !== defaultValue).map(value => ({ value, label: value }))]} />)
  return <div className="space-y-6">
    <PageHeader title={tr('OAuth gateway', 'OAuth 网关')} description={tr('Authorize accounts through the gateway and import the results into this server.', '通过网关授权账号，并将结果导入本服务。')} />
    <div className="rounded-md border border-amber-500/40 bg-amber-500/10 p-4 text-sm leading-relaxed" role="note">{tr('Manual operation only: automatic recovery after 401 is not implemented. Encrypted gateway state requires CODEX_CRED_ENCRYPTION_KEY on the server.', '仅支持手动操作：尚未实现 401 自动恢复。网关加密状态需要在服务端配置 CODEX_CRED_ENCRYPTION_KEY。')} {settings?.auto_recovery_reason}</div>
    {message && <div role="status" className="break-words rounded-md border border-border p-3 text-sm">{message}</div>}
    {pendingID && <div role="status" className="break-all rounded-md border border-border p-3 text-sm">{tr('Submitting request', '正在提交请求')}: {pendingID}. {tr('Keep this ID until the result is known.', '请保留此 ID 直到结果明确。')}</div>}
    <Card><CardHeader><CardTitle>{tr('Gateway connection', '网关连接')}</CardTitle></CardHeader><CardContent className="space-y-4">
      <div className="grid gap-4 md:grid-cols-2">{label(tr('OAuth URL', 'OAuth 地址'), <Input value={url} onChange={e => setUrl(e.target.value)} autoComplete="off" />)}{label(tr('OAuth token', 'OAuth 令牌'), <Input type="password" value={token} onChange={e => setToken(e.target.value)} autoComplete="new-password" />, settings?.oauth_token_set ? tr('Token is set; leave blank to keep it.', '已设置令牌；留空保留现有值。') : tr('Not configured', '尚未设置'))}</div>
      <div className="flex flex-wrap gap-2"><Button disabled={!!busy || !!uncertain} onClick={saveSettings}>{tr('Save settings', '保存设置')}</Button><Button variant="outline" disabled={!!busy} onClick={() => void run('test', async signal => { const d = await gatewayRequest<Connection>('/connections/test', 'POST', {}, signal); if (!signal.aborted) setConnection(`OAuth: ${d.oauth?.ok ? 'OK' : problemText(d.oauth?.error)}; Codex: ${d.codex?.ok ? 'OK' : problemText(d.codex?.error)}`) })}>{tr('Test connections', '测试连接')}</Button><Button variant="outline" disabled={!!busy || !!uncertain} onClick={loadCapabilities}>{tr('Load capabilities and quota', '读取能力和配额')}</Button></div>
      <p className="text-sm text-muted-foreground">{settings ? (settings.configured ? tr('Configured', '已配置') : tr('Not configured', '未配置')) : tr('Loading settings…', '正在读取设置…')}{connection && ` · ${connection}`}</p>
      {caps && <p className="text-sm text-muted-foreground">{tr('Quota remaining / total', '配额剩余 / 总计')}: {quota?.remaining ?? '—'} / {quota?.total ?? '—'}; {tr('reserved', '已预留')}: {quota?.reserved ?? '—'}; {tr('used', '已使用')}: {quota?.used ?? '—'}; {tr('batch limit', '批量上限')}: {caps.max_batch_size ?? '—'}; {tr('upload bytes', '上传字节上限')}: {caps.max_upload_bytes ?? '—'}</p>}
    </CardContent></Card>
    <Card><CardHeader><CardTitle>{tr('Prepare authorization', '准备授权')}</CardTitle></CardHeader><CardContent className="space-y-4">
      <fieldset disabled={busy === 'submit'} className="space-y-4">
      <p className="text-sm text-muted-foreground">{tr('Materials stay in memory and are cleared after acceptance. TXT: identifier----password----TOTP_SECRET, one per line. Use a JSON array with identifier, password, totp_secret and optional proxy for per-account proxies or passwords containing the separator. Maximum 1 MiB and 100 accounts.', '材料仅保存在内存中，任务接受后清除。TXT 每行：账号----密码----TOTP_SECRET。每账号代理或含分隔符的密码请使用 JSON 数组，字段为 identifier、password、totp_secret 和可选 proxy。最多 1 MiB、100 个账号。')}</p>
      <div className="grid gap-4 md:grid-cols-2">{label(tr('Input format', '输入格式'), <Select aria-label={tr('Input format', '输入格式')} value={format} disabled={!!uncertain} onValueChange={v => { setFormat(v as 'txt' | 'json'); invalidate() }} options={[{ value: 'txt', label: 'TXT' }, { value: 'json', label: 'JSON' }]} />)}{label(tr('UTF-8 file', 'UTF-8 文件'), <input ref={fileRef} type="file" accept=".txt,.json,text/plain,application/json" disabled={!!uncertain} className={fieldClass} onChange={e => { const file = e.target.files?.[0]; if (!file) return; setSource(''); invalidate(); const rev = revisionRef.current; if (file.size > 1048576) { setMessage(tr('File exceeds 1 MiB.', '文件超过 1 MiB。')); return } void file.arrayBuffer().then(bytes => { if (controller.current && rev === revisionRef.current) { setSource(new TextDecoder('utf-8', { fatal: true }).decode(bytes)); invalidate() } }).catch(() => setMessage(tr('File must be valid UTF-8.', '文件必须是有效 UTF-8。'))) }} />)}</div>
      {maskedText ? label(tr('Account material', '账号材料'), <textarea className={`${fieldClass} min-h-28`} style={{ WebkitTextSecurity: 'disc' } as React.CSSProperties} value={source} disabled={!!uncertain} autoComplete="off" onChange={e => { setSource(e.target.value); invalidate() }} />) : <p className="text-sm">{tr('This browser cannot mask multiline input. Import a file instead.', '当前浏览器无法遮盖多行输入，请使用文件导入。')}</p>}
      <div className="grid gap-4 md:grid-cols-2 xl:grid-cols-4">{select(tr('Country', '国家'), 'country', caps?.countries)}{select(tr('Environment', '环境'), 'environment', caps?.environments)}{select(tr('Workspace policy', '工作区策略'), 'workspace_policy', caps?.workspace_policies)}{select(tr('OAuth proxy mode', 'OAuth 代理模式'), 'proxy_mode', caps?.proxy_modes, 'system')}</div>
      {opts.proxy_mode === 'custom' && <div className="space-y-2">{label(tr('OAuth proxy URI', 'OAuth 代理 URI'), <Input type="password" value={opts.proxy || ''} disabled={!!uncertain} autoComplete="new-password" onChange={e => setOption('proxy', e.target.value)} />)}<Button variant="outline" disabled={!!busy || !caps?.features?.proxy_check || !opts.proxy} onClick={() => void run('proxy', async signal => { const options = { ...opts }; delete options.workspace_policy; const result = await gatewayRequest<ProxyCheck>('/proxy-check', 'POST', { options }, signal); if (!signal.aborted) setProxyResult(`${result.usable ? tr('Usable', '可用') : tr('Not usable', '不可用')} · ${tr('requested / observed country', '请求 / 实际地区')}: ${result.requested_country || '—'} / ${result.observed_country || '—'} · OpenAI: ${result.openai_reachable ? 'OK' : '—'} · HTTP ${result.openai_http_status ?? '—'} · Trace ${result.trace_http_status ?? '—'} ${problemText(result.error)}`) })}>{tr('Check OAuth proxy from execution machine', '从执行机器检查 OAuth 代理')}</Button>{proxyResult && <p role="status" className="text-sm break-words">{proxyResult}</p>}</div>}
      <div className="grid gap-4 md:grid-cols-2">{label(tr('Runtime proxy URI (optional)', '运行时代理 URI（可选）'), <Input type="password" value={runtimeProxy} disabled={!!uncertain} autoComplete="new-password" onChange={e => { setRuntimeProxy(e.target.value); invalidate() }} />, tr('Must already be enabled in proxy management. Blank inherits this server’s normal proxy policy; it does not copy the OAuth proxy.', '须先在代理管理中添加并启用。留空沿用本服务的正常代理策略，不会复制 OAuth 代理。'))}{label('User-Agent', <Input value={opts.user_agent || ''} disabled={!!uncertain} onChange={e => setOption('user_agent', e.target.value)} />, tr('Only for custom_ua environment.', '仅用于 custom_ua 环境。'))}{label(tr('Language (optional)', '语言（可选）'), <Input value={opts.language || ''} disabled={!!uncertain} onChange={e => setOption('language', e.target.value)} />)}{label(tr('Timezone (optional)', '时区（可选）'), <Input value={opts.timezone || ''} disabled={!!uncertain} onChange={e => setOption('timezone', e.target.value)} />)}</div>
      <fieldset disabled={!!uncertain}><legend className="mb-2 text-sm font-medium">{tr('Destination groups', '目标分组')}</legend><div className="flex flex-wrap gap-4 text-sm">{groups.length ? groups.map(g => <label key={g.id} className="flex items-center gap-2"><input type="checkbox" checked={selectedGroups.includes(g.id)} onChange={e => { setSelectedGroups(old => e.target.checked ? [...old, g.id] : old.filter(id => id !== g.id)); invalidate() }} />{g.name || g.id}</label>) : <span className="text-muted-foreground">{tr('No groups loaded.', '未读取到分组。')}</span>}</div></fieldset>
      <div className="flex flex-wrap gap-2"><Button variant="outline" disabled={!!busy || !!uncertain} onClick={() => void run('preview', async signal => { const input = makeInput(); const rev = revisionRef.current; setPreview(null); const result = await gatewayRequest<Preview>('/preview', 'POST', input, signal); if (!signal.aborted && rev === revisionRef.current) setPreview({ revision: rev, input, result }) })}>{busy === 'preview' ? tr('Checking…', '预检中…') : tr('Preview', '预检')}</Button><Button disabled={!!busy || !!uncertain || !preview?.result.valid || !previewMatches(preview, revision)} onClick={submit}>{busy === 'submit' ? tr('Submitting…', '提交中…') : tr('Create authorization job', '创建授权任务')}</Button></div>
      {preview && <div className="space-y-1 rounded-md border border-border p-3 text-sm" role="status"><p>{preview.result.valid ? tr('Preview passed', '预检通过') : tr('Preview failed', '预检未通过')} · {preview.result.count} {tr('accounts', '个账号')} · {preview.result.groups} {tr('batches', '个批次')}</p>{preview.result.rows?.map((row, i) => <p key={i}>{row.line}. {row.identifier_masked} · {row.proxy_mode}</p>)}{preview.result.errors?.map((row, i) => <p key={i}>{row.line}. {row.code}</p>)}</div>}
      {uncertain && <div className="space-y-2 rounded-md border border-amber-500/40 p-3 text-sm"><p>{tr('Uncertain request ID', '结果不确定的请求 ID')}: <span className="break-all">{uncertain}</span></p><p>{tr('Only this request ID is kept in the browser session, so returning to this page resumes reconciliation. Login material is not saved. A missing job does not prove it was not accepted; do not start another authorization.', '当前浏览器会话只保留此请求 ID，返回页面可继续核查，不保存登录材料。未找到任务并不证明提交未被接受，请勿另建授权。')}</p><Button variant="outline" disabled={!!busy} onClick={reconcile}>{tr('Find original job', '查找原任务')}</Button></div>}
      </fieldset>
    </CardContent></Card>
    <Card><CardHeader><div className="flex flex-wrap items-center justify-between gap-3"><CardTitle>{tr('Authorization jobs', '授权任务')}</CardTitle><div className="flex gap-2"><Button variant="outline" size="sm" onClick={() => setReload(n => n + 1)}>{tr('Refresh list', '刷新列表')}</Button><Button variant="outline" size="sm" aria-pressed={archived} onClick={() => setArchived(v => !v)}>{archived ? tr('Active jobs', '当前任务') : tr('Archived jobs', '归档任务')}</Button></div></div></CardHeader><CardContent className="space-y-3">
      {jobsError && <p role="alert" className="break-words text-sm">{tr('Could not load jobs:', '读取任务失败：')} {jobsError}</p>}
      {jobs === null ? <p className="text-sm text-muted-foreground">{jobsError ? tr('Use Refresh list to try again.', '请使用刷新列表重试。') : tr('Loading jobs…', '正在读取任务…')}</p> : jobs.length === 0 ? <p className="text-sm text-muted-foreground">{tr('No jobs in this view.', '此处暂无任务。')}</p> : jobs.map(job => <article key={job.id} className="space-y-3 rounded-md border border-border p-4"><div className="flex flex-wrap items-start justify-between gap-2"><div className="min-w-0"><p className="break-all text-sm font-medium">{job.id}</p><p className="text-sm text-muted-foreground">{job.state}{job.archived ? ` · ${tr('archived', '已归档')}` : ''}</p></div><p className="text-sm">{tr('Imported', '已导入')} {job.imported}/{job.total} · {tr('Failed', '失败')} {job.failed} · {tr('Pending', '待处理')} {job.pending ?? '—'}</p></div>
        {!job.archived && <><div className="flex flex-wrap gap-2">{([['refresh', job.can_refresh, tr('Refresh original', '查询原任务')], ['cancel', job.can_cancel, tr('Cancel', '取消')], ['retry-import', job.can_retry_import, tr('Retry import', '重试导入')], ['archive', job.can_archive, tr('Archive', '归档')]] as const).map(([path, allowed, text]) => <Button key={path} variant="outline" size="sm" disabled={!allowed || !!busy} onClick={() => action(job, path)}>{text}</Button>)}</div><details><summary className="cursor-pointer text-sm font-medium focus-visible:outline-2 focus-visible:outline-ring">{tr('Batch and account details', '批次与账号详情')}</summary><div className="mt-3 space-y-3">{job.groups?.length ? job.groups.map(g => <div key={g.index} className="space-y-1 border-t border-border pt-3 text-sm"><p>{tr('Group', '分组')} {g.index + 1}: {g.state} · {g.imported}/{g.total}</p><p className="break-all text-muted-foreground">{tr('Batch', '批次')}: {g.batch_id || '—'} · {tr('Request', '请求')}: {g.request_id || '—'}</p>{g.error && <p className="break-words">{problemText(g.error)}</p>}{(g.read_failures || g.next_check_at) && <p>{tr('Read operation / failures / next check', '读取操作 / 失败次数 / 下次检查')}: {g.read_operation || '—'} / {g.read_failures || 0} / {g.next_check_at || '—'}</p>}{g.items?.map((item, i) => <div key={i} className="break-words pl-3">{item.line}. {item.identifier_masked} · OAuth: {item.state} · {tr('Import', '导入')}: {item.import_state}{item.error && ` · ${problemText(item.error)}`}</div>)}</div>) : <p className="text-sm text-muted-foreground">{tr('No group details yet.', '暂无分组详情。')}</p>}</div></details></>}
      </article>)}
    </CardContent></Card>
  </div>
}
