import { useEffect, useMemo, useRef, useState, type ReactNode } from 'react'
import { Link, useLocation } from 'react-router-dom'
import { useTranslation } from 'react-i18next'
import PageHeader from '../components/PageHeader'
import { Button } from '../components/ui/button'
import { Card, CardContent, CardHeader, CardTitle } from '../components/ui/card'
import { Input } from '../components/ui/input'
import { Select } from '../components/ui/select'
import { api, PROXY_POOL_CHANGED, type ProxyRow, type ProxyTestResult } from '../api'
import { ProxyPoolSelect } from '../components/ProxyPoolSelect'
import { formatProxyDisplayURL, formatProxyIdentity, proxyIsSelectable } from '../lib/accountProxyBinding'
import { createSubmission, parseMaterial, withAccountRuntimeProxies, previewMatches, readPendingRequest, rememberPendingRequest, forgetPendingRequest, type Account, type GatewayInput, type Problem } from '../lib/oauthGateway'
import { OAuthJobs, type Job } from '../components/OAuthJobs'
import { gatewayRequest, GatewayError } from '../lib/oauthGatewayApi'

type Settings = { oauth_url: string; oauth_token_set: boolean; configured: boolean; auto_recovery_supported: boolean; auto_recovery_reason: string; max_jobs?: number; max_archived_jobs?: number }
type Capabilities = { countries?: string[]; environments?: string[]; proxy_modes?: string[]; workspace_policies?: string[]; max_batch_size?: number; max_upload_bytes?: number; features?: Record<string, boolean> }
type Quota = { remaining?: number; total?: number; reserved?: number; used?: number; disabled?: boolean }
type Group = { id: number; name: string }
type Identifier = { identifier?: string; identifier_masked?: string }
type Preview = { valid: boolean; count: number; groups: number; rows?: (Identifier & { line: number; proxy_mode: string })[]; errors?: { line: number; code: string }[] }
type Recovery = { account_id: number; identifier: string; state: string; attempts: number; next_attempt_at?: string; job_id?: string; error?: Problem; can_disable: boolean; can_reauthorize?: boolean; last_outcome?: string; last_outcome_at?: string; account_disabled?: boolean }
type Connection = { oauth: { ok: boolean; error?: Problem; capabilities?: Capabilities; quota?: Quota }; codex: { ok: boolean; error?: Problem; groups?: Group[] } }
type ProxyCheck = { usable: boolean; requested_country?: string; observed_country?: string; openai_reachable?: boolean; openai_http_status?: number; trace_http_status?: number; error?: Problem }
const fieldClass = 'w-full rounded-md border border-input bg-transparent px-3 py-2 text-sm outline-none focus-visible:ring-2 focus-visible:ring-ring'
const errorText = (e: unknown) => e instanceof Error ? e.message : 'Request failed'
const identifier = (item: Identifier) => item.identifier || item.identifier_masked || '—'
const label = (name: string, child: ReactNode, hint?: string) => <label className="flex min-w-0 flex-col gap-2 text-sm"><span className="font-medium">{name}</span>{child}{hint && <span className="text-xs text-muted-foreground">{hint}</span>}</label>

function RuntimeProxyTest({ url, disabled, zh, name }: { url: string; disabled: boolean; zh: boolean; name: string }) {
  const [result, setResult] = useState<ProxyTestResult | null>(null)
  const [testing, setTesting] = useState(false)
  const request = useRef<AbortController | null>(null)
  useEffect(() => () => request.current?.abort(), [])
  const probe = async () => {
    if (!url || request.current) return
    const controller = new AbortController()
    request.current = controller
    setTesting(true); setResult(null)
    try {
      // Omit the proxy ID: this probe must not alter the pool's health or bindings.
      const value = await api.testProxy(url, undefined, zh ? 'zh-CN' : 'en', controller.signal)
      if (!controller.signal.aborted) setResult(value)
    } catch {
      if (!controller.signal.aborted) setResult({ success: false, conclusive: false })
    } finally {
      request.current = null
      if (!controller.signal.aborted) setTesting(false)
    }
  }
  return <div className="flex flex-wrap items-center gap-2">
    <Button type="button" size="sm" variant="outline" aria-label={name} disabled={disabled || !url || testing} onClick={() => void probe()}>
      {testing ? (zh ? '正在测试…' : 'Testing…') : (zh ? '测试连通性' : 'Test connectivity')}
    </Button>
    {result && <span role="status" className={`text-xs tabular-nums ${result.success ? 'text-muted-foreground' : 'text-destructive'}`}>
      {result.success ? (zh ? '可连通' : 'Reachable') : result.conclusive ? (zh ? '不可连通' : 'Unreachable') : (zh ? '未能确认连通性' : 'Connectivity not confirmed')}
      {result.latency_ms != null && ` · ${result.latency_ms} ms`}
    </span>}
  </div>
}

export default function OAuthGateway({ embedded = false, active = true }: { embedded?: boolean; active?: boolean }) {
  const { hash } = useLocation()
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
  const [runtimeProxies, setRuntimeProxies] = useState<ProxyRow[] | null>(null)
  const [accountRuntimeProxies, setAccountRuntimeProxies] = useState<Record<string, string>>({})
  const [saveCredentials, setSaveCredentials] = useState(false)
  const [recoveryAccountID, setRecoveryAccountID] = useState('')
  const [retry, setRetry] = useState<{ job_id: string; line: number; identifier: string } | null>(null)
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
  const [batchArchiving, setBatchArchiving] = useState(false)
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
  const material = useMemo(() => {
    if (!source.trim()) return { accounts: [] as Account[], error: '' }
    try { return { accounts: parseMaterial(source, format), error: '' } }
    catch (e) { return { accounts: [] as Account[], error: errorText(e) } }
  }, [source, format])
  const runtimeAccounts = withAccountRuntimeProxies(material.accounts, accountRuntimeProxies)
  const runtimeProxyLabel = (value: string) => {
    if (!value) return tr('Inherit the server proxy policy', '沿用服务正常代理策略')
    const selected = runtimeProxies?.find(p => p.url === value)
    return selected ? `${formatProxyIdentity(selected)} (${formatProxyDisplayURL(value)})` : formatProxyDisplayURL(value)
  }
  const runtimeProxyTest = (value: string, name: string) => {
    const url = value.trim()
    return <RuntimeProxyTest key={url} url={url} disabled={formLocked || repairing} zh={zh} name={name} />
  }

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
    setSource(''); setRuntimeProxy(''); setAccountRuntimeProxies({}); setOpts(current => ({ ...current, proxy: '' }))
    setRetry(null)
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
  const loadRuntimeProxies = () => run('runtime-proxies', async signal => {
    const data = await api.listProxies()
    if (!signal.aborted) setRuntimeProxies(data.proxies || [])
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
    void loadSettings(); void loadRuntimeProxies()
    const reloadProxies = () => { void loadRuntimeProxies() }
    window.addEventListener(PROXY_POOL_CHANGED, reloadProxies)
    window.addEventListener('focus', reloadProxies)
    return () => { window.removeEventListener(PROXY_POOL_CHANGED, reloadProxies); window.removeEventListener('focus', reloadProxies); abort.abort(); controller.current = null; revisionRef.current++ }
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
  const setAccountRuntimeProxy = (account: Account, value: string) => {
    if (formLocked || repairing) return
    setAccountRuntimeProxies(old => ({ ...old, [account.identifier.trim().toLowerCase()]: value }))
    invalidate()
  }
  const inputProblem = () => {
    if (uncertain) return tr('Resolve the original submission below before creating another job.', '先核查下方原请求，结果明确前不能创建新任务。')
    if (!settings) return tr('Load gateway settings first.', '请先读取网关设置；读取失败时使用上方重试。')
    if (!settings.configured) return tr('Save the gateway URL and customer CDK first.', '请先保存网关地址和客户 CDK。')
    if (settingsDirty) return tr('Connection settings changed. Save or restore them first.', '连接设置尚未保存，请先保存或还原输入。')
    if (!caps) return tr('Capabilities are not loaded. Retry above.', '服务能力尚未读取，请在上方重试读取能力。')
    if (!source.trim()) return tr('Paste account material or choose a UTF-8 file.', '请粘贴账号材料，或选择 UTF-8 文件。')
    if (material.error) return tr('Fix the material format: ', '请修正材料格式：') + material.error
    const count = material.accounts.length
    if (repairing && (!/^[1-9]\d*$/.test(recoveryAccountID) || !Number.isSafeInteger(Number(recoveryAccountID)))) return tr('Use a positive integer account ID.', '原账号 ID 必须是正整数。')
    if (repairing && count !== 1) return tr('Repairing an existing account requires exactly one account.', '修复已有账号时只能提供一个账号的材料。')
    if (retry && (count !== 1 || material.accounts[0].identifier.trim().toLowerCase() !== retry.identifier.trim().toLowerCase())) return tr('Re-enter exactly one account matching the original identifier.', '请重新填写且仅填写与原标识匹配的一个账号。')
    if ((saveCredentials || repairing) && !settings.auto_recovery_supported) return tr('This server does not support controlled account recovery.', '当前服务不支持受控账号恢复，请先升级服务端。')
    if ((repairing || saveCredentials) && !caps.features?.expected_identity_guard) return tr('The gateway cannot verify the original identity; repair and automatic recovery are unavailable.', '网关未提供原身份校验能力，不能修复原账号或启用自动恢复。')
    if (opts.proxy_mode === 'custom' && !opts.proxy) return tr('Enter the OAuth proxy URI or use the gateway dynamic proxy.', '请输入自带 OAuth 代理 URI，或改用网关动态代理。')
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
    const accounts = withAccountRuntimeProxies(material.accounts, accountRuntimeProxies, repairing)
    const options = Object.fromEntries(Object.entries(opts).filter(([key, value]) => value && (key !== 'proxy' || opts.proxy_mode === 'custom')))
    return { ...(format === 'json' || accounts.some(a => a.runtime_proxy) ? { accounts } : { source_text: source.replace(/^\uFEFF/, '') }), options, group_ids: repairing ? [] : selectedGroups, save_credentials: saveCredentials, ...(repairing ? { recovery_account_id: Number(recoveryAccountID) } : runtimeProxy ? { runtime_proxy: runtimeProxy } : {}), ...(retry ? { retry_of: { job_id: retry.job_id, line: retry.line } } : {}) }
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
    if (retry && !window.confirm(tr(`Create a new authorization for ${retry.identifier} from job ${retry.job_id}, line ${retry.line}?`, `确认重新授权 ${retry.identifier}（原任务 ${retry.job_id}，第 ${retry.line} 行）？`))) return
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
  const archiveDescription = tr('This clears task details and any remaining task material. Saved automatic recovery is not changed.', '归档会清除任务详情及剩余任务材料，保留结果摘要；已启用的自动恢复不受影响。')
  const archiveJob = (job: Job, signal: AbortSignal) => gatewayRequest(`/jobs/${encodeURIComponent(job.id)}/archive`, 'POST', { discard_credentials: job.retained_material }, signal)
  const action = (job: Job, path: string) => run(`job:${job.id}`, async signal => {
    if (path === 'archive') {
      if (!window.confirm(archiveDescription)) return
      await archiveJob(job, signal)
    } else await gatewayRequest(`/jobs/${encodeURIComponent(job.id)}/${path}`, 'POST', {}, signal)
    if (!signal.aborted) setReload(n => n + 1)
  })
  const archivePage = (items: Job[]) => run('archive-page', async signal => {
    if (!items.length || !window.confirm(tr(`Archive ${items.length} resolved jobs on this page?`, `归档本页 ${items.length} 个已结束任务？`) + '\n' + archiveDescription)) return
    setBatchArchiving(true)
    let done = 0
    try {
      for (const job of items) {
        if (signal.aborted) return
        await archiveJob(job, signal)
        done++
      }
    } finally {
      if (!signal.aborted) {
        setBatchArchiving(false)
        setMessage(tr(`Archived ${done}/${items.length} jobs.`, `已归档 ${done}/${items.length} 个任务。`))
        setReload(n => n + 1)
      }
    }
  })
  const reauthorizeSaved = (entry: Recovery) => run('submit', async signal => {
    if (formLocked || !entry.can_reauthorize || entry.account_disabled || busy.current.has(`recovery:${entry.account_id}`)) return
    if (!window.confirm(tr(`Reauthorize account #${entry.account_id} with its saved login material? A successful authorization can use one gateway credit.`, `使用已保管材料重新授权账号 #${entry.account_id}？成功会消耗一次网关额度。`))) return
    if (!globalThis.crypto?.randomUUID) throw new Error(tr('Use HTTPS to generate a safe request ID.', '请使用 HTTPS 打开后台，以生成安全的请求 ID。'))
    const id = `saved-${crypto.randomUUID()}`
    rememberPendingRequest(id); setPendingID(id)
    try {
      const job = await gatewayRequest<Job>(`/recovery/${entry.account_id}/reauthorize`, 'POST', { request_id: id }, signal)
      if (signal.aborted) return
      if (job?.id !== id) throw new GatewayError(200, { message: 'Unexpected job response' }, true)
      forgetPendingRequest()
      setMessage(tr(`Reauthorization queued: ${job.id}`, `重新授权已排队：${job.id}`)); setArchived(false); setReload(n => n + 1)
    } catch (e) {
      if (signal.aborted) return
      if (e instanceof GatewayError && e.uncertain) setUncertain(id)
      else { forgetPendingRequest(); throw e }
    } finally { if (!signal.aborted) setPendingID('') }
  })
  const disableRecovery = (entry: Recovery) => run(`recovery:${entry.account_id}`, async signal => {
    if (!window.confirm(tr(`Stop automatic recovery for account #${entry.account_id} and discard its saved login material?`, `停止账号 #${entry.account_id} 的自动恢复并清除保管的登录材料？`))) return
    await gatewayRequest(`/recovery/${entry.account_id}/disable`, 'POST', {}, signal)
    if (!signal.aborted) setReload(n => n + 1)
  })
  const startRetry = (job: Job, item: Identifier & { line: number }) => {
    if (formLocked || busy.current.has('submit') || pending.preview || !item.identifier || !Number.isSafeInteger(item.line) || item.line < 1) return
    if ((source || preview || retry) && !window.confirm(tr('Discard the current unsubmitted material and preview?', '放弃当前未提交的材料和预检结果？'))) return
    clearMaterial(); setFormat('txt'); setOpts({ proxy_mode: 'system' }); setSelectedGroups([]); setRecoveryAccountID(''); setSaveCredentials(false); setProxyResult('')
    setRetry({ job_id: job.id, line: item.line, identifier: item.identifier })
  }
  const select = (name: string, key: string, values?: string[], fallback = '') => {
    const countryName = (value: string) => {
      if (!/^[A-Z]{2}$/.test(value)) return value
      try { return `${new Intl.DisplayNames([zh ? 'zh-CN' : 'en'], { type: 'region' }).of(value)} (${value})` } catch { return value }
    }
    const options = [{ value: fallback, label: fallback === 'system' ? tr('Gateway dynamic proxy', '网关动态代理') : key === 'workspace_policy' ? tr('Team preferred (default)', '团队优先（默认）') : tr('Automatic (default)', '自动（默认）') }, ...(values || []).filter(v => v !== fallback).map(value => ({ value, label: key === 'country' ? countryName(value) : key === 'proxy_mode' && value === 'custom' ? tr('Custom OAuth proxy', '自带 OAuth 代理') : value }))]
    if (opts[key] && !options.some(v => v.value === opts[key])) options.push({ value: opts[key], label: `${opts[key]} (${tr('unavailable', '已失效')})` })
    return label(name, <Select aria-label={name} value={opts[key] || fallback} disabled={!caps || formLocked} onValueChange={value => setOption(key, value)} options={options} />)
  }
  const showError = (key: string) => errors[key] ? <p role="alert" className="break-words text-sm text-destructive">{errors[key]}</p> : null
  const description = tr('Load connection data, check account material, then authorize. Job results and recovery stay visible below.', '读取连接信息、预检账号材料后再授权。任务结果和自动恢复状态都在下方查看。')

  const anchorReady = hash !== '#oauth-recovery' || settings?.auto_recovery_supported
  useEffect(() => {
    if (active && hash && anchorReady) document.getElementById(hash.slice(1))?.scrollIntoView()
  }, [active, anchorReady, hash])

  return <div className="space-y-6">
    {embedded ? (
      <p className="text-sm text-muted-foreground">{description}</p>
    ) : (
      <PageHeader title={tr('OAuth gateway', 'OAuth 网关')} description={description} />
    )}
    <nav aria-label={tr('OAuth gateway sections', 'OAuth 网关页面导航')} className="flex flex-wrap gap-x-5 gap-y-2 text-sm text-primary">
      <a className="underline underline-offset-4" href="#oauth-prepare">{tr('Prepare authorization', '准备授权')}</a>
      <a className="underline underline-offset-4" href="#oauth-jobs">{tr('Authorization jobs', '授权任务')}</a>
      {settings?.auto_recovery_supported && <a className="underline underline-offset-4" href="#oauth-recovery">{tr('Automatic account recovery', '账号自动恢复')}</a>}
    </nav>
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
    <Card id="oauth-prepare" className="scroll-mt-6"><CardHeader><CardTitle>{tr('Prepare authorization', '准备授权')}</CardTitle></CardHeader><CardContent className="space-y-4">
      {retry && <div role="status" className="space-y-2 rounded-md border border-amber-500/40 p-3 text-sm"><p>{tr('Reauthorize one account; re-enter its material:', '重新授权单个账号，请重新填写该账号材料：')} {retry.identifier} · {retry.job_id} · {tr('Line', '第')} {retry.line} {tr('', '行')}</p><Button variant="outline" disabled={formLocked || !!pending.preview} onClick={() => { setRetry(null); invalidate() }}>{tr('Cancel reauthorization', '取消重新授权')}</Button></div>}
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
        <section className="space-y-3 border-t border-border pt-4" aria-labelledby="oauth-exit-heading">
        <h3 id="oauth-exit-heading" className="text-sm font-semibold">{tr('OAuth authorization exit', 'OAuth 授权出口')}</h3>
        <p className="text-xs text-muted-foreground">{tr('Used only during authorization. The gateway assigns the dynamic proxy. Country choices come from the gateway; they are not proof of the actual exit.', '仅用于授权。动态代理由 OAuth 网关分配，不是 Codex2API 的运行代理。国家／地区来自网关能力列表，不代表实际出口已验证。')}</p>
        <div className="grid gap-4 md:grid-cols-2 xl:grid-cols-4">{select(tr('OAuth exit country or region', '授权出口国家／地区'), 'country', caps?.countries)}{select(tr('Environment', '环境'), 'environment', caps?.environments)}{select(tr('Workspace policy', '工作区策略'), 'workspace_policy', caps?.workspace_policies)}{select(tr('OAuth proxy mode', 'OAuth 代理模式'), 'proxy_mode', caps?.proxy_modes, 'system')}</div>
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
          {label('User-Agent', <Input value={opts.user_agent || ''} disabled={opts.environment !== 'custom_ua' && !opts.user_agent} onChange={e => setOption('user_agent', e.target.value)} />, tr('Only custom_ua allows this field. Otherwise leave it blank.', '仅 custom_ua 环境需要填写，其他环境请留空。'))}
          {label(tr('Language (optional)', '语言（可选）'), <Input value={opts.language || ''} onChange={e => setOption('language', e.target.value)} />)}
          {label(tr('Timezone (optional)', '时区（可选）'), <Input value={opts.timezone || ''} onChange={e => setOption('timezone', e.target.value)} />)}
        </div>
        </section>
        <section className="space-y-3 border-t border-border pt-4" aria-labelledby="runtime-proxy-heading">
          <div className="flex flex-wrap items-center justify-between gap-2"><h3 id="runtime-proxy-heading" className="text-sm font-semibold">{tr('Runtime proxy for imported accounts', '导入后的运行代理')}</h3><Button variant="outline" size="sm" disabled={!!pending['runtime-proxies']} onClick={loadRuntimeProxies}>{pending['runtime-proxies'] ? tr('Loading proxies…', '正在读取代理…') : tr('Refresh proxy list', '刷新代理列表')}</Button></div>
          <div className="grid gap-2 md:grid-cols-2">
            {label(tr('Batch runtime proxy (optional)', '本次批量运行代理（可选）'), <Input aria-label={tr('Batch runtime proxy URI', '本次批量运行代理 URI')} type="password" value={runtimeProxy} disabled={repairing} autoComplete="new-password" onChange={e => { setRuntimeProxy(e.target.value); invalidate() }} />, repairing ? tr('Keep the original account proxy and groups.', '修复已有账号时保留原代理和分组。') : tr('Default for every account in this import, unless explicitly overridden below. Used for model requests only. Blank uses server policy.', '适用于本次导入的所有账号，除非下方单独覆盖。只用于导入后的模型请求；留空沿用服务器策略。'))}
            <div className="space-y-2"><span className="text-sm font-medium">{tr('Choose a batch proxy from the pool', '从代理池选择批量代理')}</span><ProxyPoolSelect proxies={runtimeProxies || []} value={runtimeProxy} disabled={formLocked || repairing} onSelect={value => { if (!formLocked && !repairing) { setRuntimeProxy(value); invalidate() } }} />{runtimeProxyTest(runtimeProxy, tr('Test batch proxy connectivity', '测试批量代理连通性'))}</div>
          </div>
          <p className="break-words text-xs text-muted-foreground">{tr('Batch default', '本次默认')}: {repairing ? tr('Keep the original account proxy', '保留原账号代理') : runtimeProxyLabel(runtimeProxy)}</p>
          <Link className="inline-block text-xs text-primary underline underline-offset-4" to="/proxies" target="_blank" rel="noopener noreferrer">{tr('Add or manage proxies in a new tab', '在新标签页添加或管理代理')}</Link>
          {showError('runtime-proxies')}{errors['runtime-proxies'] && <p className="text-xs text-muted-foreground">{tr('You can still enter a registered URI; preview will verify it on the server.', '仍可填写已登记的代理 URI，预检时由服务端校验。')}</p>}
          {runtimeProxies?.length === 0 && !errors['runtime-proxies'] && <p className="text-xs text-muted-foreground">{tr('No proxies are registered yet. Add one in proxy management to assign it here.', '尚未登记代理，请先到代理管理添加，再回这里选择。')}</p>}
          <details className="space-y-3 rounded-md border border-border p-3">
            <summary className="cursor-pointer text-sm font-medium focus-visible:outline-2 focus-visible:outline-ring">{tr('Per-account override', '单账号覆盖')}{!repairing && ` (${runtimeAccounts.filter(account => account.runtime_proxy).length})`}</summary>
            <p className="text-xs text-muted-foreground">{repairing ? tr('Repair skips both these settings and runtime_proxy in the material.', '修复不会应用这里的设置或材料里的 runtime_proxy。') : tr('Set only exceptions. Blank inherits the batch default; only registered, available proxies may be used.', '只设置例外；空白继承本次批量代理。代理须已登记且允许该账号使用。')}</p>
          {!runtimeAccounts.length ? <p className="text-sm text-muted-foreground">{tr('Enter valid TXT or JSON material to configure each account here.', '填写有效的 TXT 或 JSON 材料后，即可在这里逐个设置。')}</p> : <div className="divide-y divide-border">{runtimeAccounts.map((account, index) => <div key={`${account.identifier.trim().toLowerCase()}:${index}`} role="group" aria-label={tr(`Runtime proxy for ${account.identifier}`, `账号 ${account.identifier} 的运行代理`)} className="grid gap-3 py-3 md:grid-cols-[minmax(0,1fr)_minmax(0,2fr)]">
            <p className="min-w-0 break-all text-sm">{index + 1}. {account.identifier}</p>
            <div className="min-w-0 space-y-2"><div className="grid gap-2 sm:grid-cols-2"><Input aria-label={tr(`Runtime proxy URI for ${account.identifier}`, `账号 ${account.identifier} 的运行代理 URI`)} type="password" autoComplete="new-password" value={account.runtime_proxy || ''} disabled={repairing} placeholder={tr('Blank inherits the default above', '留空沿用上方统一设置')} onChange={e => setAccountRuntimeProxy(account, e.target.value)} /><ProxyPoolSelect proxies={runtimeProxies || []} value={account.runtime_proxy || ''} disabled={formLocked || repairing} onSelect={value => setAccountRuntimeProxy(account, value)} /></div><div className="flex flex-wrap items-center gap-2"><p className="min-w-0 flex-1 break-all text-xs text-muted-foreground">{tr('Effective setting', '生效设置')}: {repairing ? tr('Keep the original account proxy', '保留原账号代理') : runtimeProxyLabel(account.runtime_proxy || runtimeProxy)}</p><Button size="sm" variant="outline" disabled={repairing || !account.runtime_proxy} onClick={() => setAccountRuntimeProxy(account, '')}>{tr('Inherit default', '沿用统一设置')}</Button></div>
              {runtimeProxyTest(account.runtime_proxy || runtimeProxy, tr(`Test proxy connectivity for ${account.identifier}`, `测试账号 ${account.identifier} 的代理连通性`))}
              {!repairing && runtimeProxies && !errors['runtime-proxies'] && (account.runtime_proxy || runtimeProxy) && !runtimeProxies.some(p => p.url === (account.runtime_proxy || runtimeProxy) && proxyIsSelectable(p)) && <p className="text-xs text-destructive">{tr('This proxy is unavailable to new accounts; correct it before preview.', '该代理当前不可用于新账号，请修正后再预检。')}</p>}</div>
          </div>)}</div>}
          </details>
        </section>
        <fieldset disabled={repairing} className="space-y-2"><legend className="mb-2 text-sm font-medium">{tr('Destination groups (optional)', '目标分组（可选）')}</legend>
          <p className="text-xs text-muted-foreground">{repairing ? tr('Repair preserves the original groups; selections here are not applied.', '修复已有账号时保留原分组，不使用此处选择。') : tr('Codex groups only. Leave unselected to import without group bindings.', '只列出 Codex 分组。不选择则导入后不绑定分组。')}</p>
          <div className="flex flex-wrap gap-4 text-sm">{groups?.length ? groups.map(g => <label key={g.id} className="flex items-center gap-2"><input type="checkbox" checked={selectedGroups.includes(g.id)} onChange={e => { setSelectedGroups(old => e.target.checked ? [...old, g.id] : old.filter(id => id !== g.id)); invalidate() }} />{g.name || `#${g.id}`}</label>) : <span className="text-muted-foreground">{pending.groups ? tr('Loading groups…', '正在读取分组…') : errors.groups ? tr('Groups could not be loaded; retry below.', '分组读取失败，请在下方重试。') : groups ? tr('No Codex groups exist. You can continue without a group.', '暂无 Codex 分组，可以不分组继续。') : tr('Groups have not been loaded yet.', '分组尚未读取。')}</span>}</div>
          {showError('groups')}{errors.groups && <Button variant="outline" size="sm" disabled={!!pending.groups} onClick={loadGroups}>{tr('Retry groups', '重试读取分组')}</Button>}
          {!!invalidGroups.length && <div className="space-y-2 text-sm"><p role="alert">{tr('Unavailable selections', '已失效的分组选择')}: {invalidGroups.map(id => `#${id}`).join(', ')}</p><Button size="sm" variant="outline" onClick={() => { setSelectedGroups(old => old.filter(id => !invalidGroups.includes(id))); invalidate() }}>{tr('Remove unavailable selections', '清除失效选择')}</Button></div>}
          <Link className="inline-block text-xs text-primary underline underline-offset-4" to="/accounts">{tr('Manage groups in Accounts', '前往账号管理查看或创建分组')}</Link>
        </fieldset>
        <div className="space-y-3 border-t border-border pt-4">
          {label(tr('Existing account ID to repair (optional)', '修复已有账号 ID（可选）'), <Input inputMode="numeric" value={recoveryAccountID} disabled={!!retry || (!settings?.auto_recovery_supported && !recoveryAccountID)} placeholder={tr('Blank creates a new account', '留空表示新增账号')} onChange={e => { setRecoveryAccountID(e.target.value.trim()); invalidate() }} />, tr('Specify the original account ID and one account’s material. Identity, workspace and credential version must match. Deleted or disabled accounts are not revived.', '填写原账号 ID，并只提供一条材料。必须通过身份、工作区与凭据版本校验；不会复活已删除或禁用账号。'))}
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
    <OAuthJobs jobs={jobs} groups={groups || []} archived={archived} error={jobsError} maxJobs={archived ? settings?.max_archived_jobs : settings?.max_jobs} busy={pending} batchArchiving={batchArchiving} tr={tr} stateText={stateText} problemText={problemText} onRefresh={() => setReload(n => n + 1)} onView={value => { if (archived !== value) { setJobs(null); setJobsError(''); setArchived(value) } }} onAction={action} onArchivePage={archivePage} actionError={key => errors[key]} onReauthorize={startRetry} reauthorizeDisabled={formLocked || !!pending.preview} />
    {settings?.auto_recovery_supported && <Card id="oauth-recovery" className="scroll-mt-6"><CardHeader><div className="flex flex-wrap items-center justify-between gap-3"><CardTitle>{tr('Automatic account recovery', '账号自动恢复')}</CardTitle><Button size="sm" variant="outline" onClick={() => setReload(n => n + 1)}>{tr('Refresh recovery status', '刷新恢复状态')}</Button></div></CardHeader><CardContent className="space-y-3">
      <p className="text-sm text-muted-foreground">{tr('Only accounts explicitly enrolled above are listed. Recovery keeps the original ID, proxy and groups; identity changes or unknown results require review.', '这里只列出明确启用的账号。恢复保留原 ID、代理和分组；身份变化或结果未知时必须核查。')}</p>
      {recoveryError && <p role="alert" className="break-words text-sm text-destructive">{recoveryError}</p>}
      {recoveries === null ? <p className="text-sm text-muted-foreground">{recoveryError ? tr('Retry with Refresh recovery status.', '请点击刷新恢复状态重试。') : tr('Loading recovery status…', '正在读取恢复状态…')}</p> : !recoveries.length ? <p className="text-sm text-muted-foreground">{tr('No enrolled accounts. Opt in when authorizing or repairing an account.', '暂无登记记录。在上方新增或修复账号时，可勾选加密保管材料。')}</p> : recoveries.map(entry => <div key={entry.account_id} className="space-y-2 border-t border-border pt-3 text-sm"><div className="flex flex-wrap items-center justify-between gap-3"><p className="min-w-0 break-all">#{entry.account_id} {entry.identifier}</p><div className="flex flex-wrap gap-2">{entry.can_reauthorize !== undefined && <Button variant="outline" size="sm" disabled={!entry.can_reauthorize || entry.account_disabled || formLocked || !!pending[`recovery:${entry.account_id}`]} onClick={() => reauthorizeSaved(entry)}>{tr('Reauthorize with saved material', '手动重新授权')}</Button>}<Button variant="outline" size="sm" disabled={!entry.can_disable || !!pending[`recovery:${entry.account_id}`]} onClick={() => disableRecovery(entry)}>{pending[`recovery:${entry.account_id}`] ? tr('Stopping…', '正在停止…') : tr('Stop recovery and clear material', '停止恢复并清除材料')}</Button></div></div>
        <p>{entry.state === 'ready' ? tr('Standing by', '待命') : stateText(entry.state)} · {tr('Reauthorization attempts', '重授权次数')}: {entry.attempts}{entry.next_attempt_at ? ` · ${tr('Next check', '下次检查')}: ${new Date(entry.next_attempt_at).toLocaleString()}` : ''}</p>
        {entry.account_disabled && <p className="text-muted-foreground">{tr('Account disabled; recovery will not enable it.', '账号已禁用；恢复不会自动启用账号。')}</p>}
        {entry.last_outcome && <p>{tr('Last recovery result', '最近恢复结果')}: {entry.last_outcome === 'refresh_succeeded' ? tr('Token refresh succeeded', '普通令牌刷新成功') : entry.last_outcome === 'reauthorization_succeeded' ? tr('Reauthorization succeeded', '重授权成功') : stateText(entry.last_outcome)}{entry.last_outcome_at && ` · ${new Date(entry.last_outcome_at).toLocaleString()}`}</p>}
        {!entry.can_disable && <p className="text-xs text-muted-foreground">{tr('Recovery is disabled for this record.', '这条记录已停止恢复。')}</p>}{entry.job_id && <p className="break-all">{tr('Linked job', '关联任务')}: <a className="text-primary underline" href={`#oauth-job-${encodeURIComponent(entry.job_id)}`}>{entry.job_id}</a></p>}{entry.error && <p className="break-words text-destructive">{problemText(entry.error)}</p>}{showError(`recovery:${entry.account_id}`)}</div>)}
    </CardContent></Card>}
  </div>
}
