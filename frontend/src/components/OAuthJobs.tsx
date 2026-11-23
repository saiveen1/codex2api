import { useMemo, useState } from 'react'
import { Link } from 'react-router-dom'
import { Button } from './ui/button'
import { Card, CardContent, CardHeader, CardTitle } from './ui/card'
import { Input } from './ui/input'
import { Select } from './ui/select'
import type { Problem } from '../lib/oauthGateway'

type JobItem = { identifier?: string; identifier_masked?: string; line: number; state: string; import_state: string; can_reauthorize?: boolean; diagnostic?: { upstream_code?: string; http_status?: number; endpoint?: string; stage?: string }; error?: Problem }
type AccountResult = { line: number; account_id?: number; plan_type?: string; workspace_id?: string; workspace_name?: string; group_ids?: number[]; runtime_proxy_id?: number; recovery_state?: string; identifier_masked?: string; outcome?: string }
export type Job = {
  id: string; state: string; total: number; imported: number; failed: number; pending: number
  can_refresh: boolean; can_cancel: boolean; can_retry_import: boolean; can_archive: boolean; retained_material: boolean; archived: boolean
  created_at?: string; archived_at?: string; job_type?: string; group_ids?: number[]; requested_country?: string; observed_country?: string
  account_results?: AccountResult[]; recovery_account_id?: number; retry_of?: { job_id: string; line: number }; auto_recovery?: boolean
  groups?: { index: number; state: string; batch_id: string; request_id: string; imported: number; total: number; error?: Problem; read_operation?: string; read_failures?: number; next_check_at?: string; items?: JobItem[] }[]
}

type Props = {
  jobs: Job[] | null; groups: { id: number; name: string }[]; archived: boolean; error: string; maxJobs?: number
  busy: Record<string, boolean>; batchArchiving: boolean; tr: (en: string, zh: string) => string
  stateText: (state: string) => string; problemText: (problem?: Problem) => string
  onRefresh: () => void; onView: (archived: boolean) => void; onAction: (job: Job, path: string) => void
  onArchivePage: (jobs: Job[]) => void; actionError: (key: string) => string | undefined
  onReauthorize: (job: Job, item: JobItem) => void; reauthorizeDisabled: boolean
}

const kinds = [
  ['import', 'New import', '新增导入'], ['manual_recovery', 'Manual repair', '手动修复'],
  ['reauthorize', 'Reauthorization', '失败项重新授权'], ['auto_recovery', '401 auto-recovery', '401 自动恢复'],
  ['unknown', 'Unknown type', '类型未知'],
] as const
const jobKind = (job: Job) => job.job_type || (job.auto_recovery ? 'auto_recovery' : job.recovery_account_id ? 'manual_recovery' : job.retry_of ? 'reauthorize' : job.archived ? 'unknown' : 'import')
const unresolved = (job: Job) => job.pending > 0 || job.can_refresh || job.can_retry_import || job.state === 'needs_review'
const timestamp = (value?: string) => value ? new Date(value).toLocaleString() : ''

export function OAuthJobs({ jobs, groups, archived, error, maxJobs, busy, batchArchiving, tr, stateText, problemText, onRefresh, onView, onAction, onArchivePage, actionError, onReauthorize, reauthorizeDisabled }: Props) {
  const [query, setQuery] = useState('')
  const [kind, setKind] = useState('')
  const [status, setStatus] = useState('')
  const [group, setGroup] = useState('')
  const [page, setPage] = useState(1)
  const [pageSize, setPageSize] = useState(10)
  const [expanded, setExpanded] = useState<Record<string, boolean>>({})
  const kindOptions = kinds.map(([value, en, zh]) => ({ value, label: tr(en, zh) }))
  const groupLabel = (id: number) => groups.find(row => row.id === id)?.name || `#${id}`
  const groupIDs = [...new Set([...groups.map(row => row.id), ...(jobs || []).flatMap(job => job.group_ids || [])])]
  const states = [...new Set((jobs || []).map(job => job.state).concat(status ? [status] : []))]
  const filtered = useMemo(() => {
    const term = query.trim().toLowerCase()
    const accountTerm = term.replace(/^#/, '')
    return (jobs || []).filter(job => {
      if (kind && jobKind(job) !== kind || status && job.state !== status) return false
      if (group === 'ungrouped' ? job.group_ids?.length !== 0 : group && !job.group_ids?.includes(Number(group))) return false
      if (!term || job.id.toLowerCase().includes(term) || String(job.recovery_account_id ?? '') === accountTerm) return true
      return job.account_results?.some(row => (row.identifier_masked || '').toLowerCase().includes(term) || String(row.account_id ?? '') === accountTerm)
        || job.groups?.some(batch => batch.items?.some(row => `${row.identifier || ''} ${row.identifier_masked || ''}`.toLowerCase().includes(term)))
    }).sort((a, b) => Number(!archived && unresolved(b)) - Number(!archived && unresolved(a)) || (b.created_at || '').localeCompare(a.created_at || ''))
  }, [jobs, query, kind, status, group, archived])
  const pages = Math.max(1, Math.ceil(filtered.length / pageSize))
  const current = Math.min(page, pages)
  const visible = filtered.slice((current - 1) * pageSize, current * pageSize)
  const archivable = visible.filter(job => job.can_archive && !busy[`job:${job.id}`])
  const archivePending = batchArchiving || !!busy['archive-page']
  const resetPage = (setter: (value: string) => void) => (value: string) => { setter(value); setPage(1) }
  const unknown = tr('Unknown', '未知')
  const all = (en: string, zh: string) => ({ value: '', label: tr(en, zh) })
  const accountLink = (id: number) => <Link className="text-primary underline underline-offset-4" to={`/accounts?account_id=${id}`} target="_blank" rel="noopener noreferrer">#{id}</Link>
  const needsReview = (state: string) => ['needs_review', 'submit_unknown', 'import_unknown'].includes(state)

  const resultDetails = (row: AccountResult) => {
    const enrollment = row.recovery_state === 'ready' ? tr('Enrolled', '已登记')
      : row.recovery_state === 'not_enrolled' ? tr('Not enrolled', '未登记')
      : row.recovery_state === 'retention_failed' ? tr('Could not save recovery material', '保管材料失败')
      : row.recovery_state ? stateText(row.recovery_state) : unknown
    const values = [
      [tr('Plan at import', '入库套餐'), row.plan_type || unknown],
      [tr('Workspace', '实际工作区'), [row.workspace_name, row.workspace_id].filter(Boolean).join(' · ') || unknown],
      [tr('Imported groups', '落地分组'), row.group_ids ? row.group_ids.map(groupLabel).join('、') || tr('No group', '无分组') : unknown],
      [tr('Bound runtime proxy', '绑定的运行代理'), row.runtime_proxy_id ? `#${row.runtime_proxy_id}` : row.runtime_proxy_id === 0 ? tr('No dedicated binding', '未单独绑定') : unknown],
      [tr('Recovery at import', '入库时的自动恢复'), enrollment],
    ]
    return <dl className="mt-2 grid gap-x-4 gap-y-1 text-xs sm:grid-cols-2 xl:grid-cols-3">{values.map(([name, value]) => <div key={name} className="min-w-0 break-words"><dt className="inline text-muted-foreground">{name}：</dt><dd className="inline">{value}</dd></div>)}</dl>
  }

  return <Card id="oauth-jobs" className="scroll-mt-6">
    <CardHeader><div className="flex flex-wrap items-center justify-between gap-3">
      <CardTitle>{tr('Authorization jobs', '授权任务')}</CardTitle>
      <div className="flex flex-wrap gap-2">
        <Button variant={archived ? 'outline' : 'default'} size="sm" aria-pressed={!archived} onClick={() => { if (archived) { setPage(1); onView(false) } }}>{tr('Current jobs', '当前任务')}</Button>
        <Button variant={archived ? 'default' : 'outline'} size="sm" aria-pressed={archived} onClick={() => { if (!archived) { setPage(1); onView(true) } }}>{tr('Archived jobs', '归档任务')}</Button>
        <Button variant="outline" size="sm" onClick={onRefresh}>{tr('Refresh list', '刷新列表')}</Button>
      </div>
    </div></CardHeader>
    <CardContent className="space-y-3">
      <div className="grid gap-2 sm:grid-cols-2 xl:grid-cols-[2fr_1fr_1fr_1fr]">
        <Input aria-label={tr('Search jobs or accounts', '搜索任务或账号')} placeholder={tr('Search task ID or account', '搜索任务 ID 或账号')} value={query} onChange={e => resetPage(setQuery)(e.target.value)} />
        <Select aria-label={tr('Job type', '任务类型')} value={kind} onValueChange={resetPage(setKind)} options={[all('All types', '所有类型'), ...kindOptions]} />
        <Select aria-label={tr('Job status', '任务状态')} value={status} onValueChange={resetPage(setStatus)} options={[all('All statuses', '所有状态'), ...states.map(value => ({ value, label: stateText(value) }))]} />
        <Select aria-label={tr('Destination group', '任务目标分组')} value={group} onValueChange={resetPage(setGroup)} options={[all('All destination groups', '所有目标分组'), { value: 'ungrouped', label: tr('No destination group', '未选目标分组') }, ...groupIDs.map(id => ({ value: String(id), label: groupLabel(id) })), ...(group && group !== 'ungrouped' && !groupIDs.includes(Number(group)) ? [{ value: group, label: `#${group}` }] : [])]} />
      </div>
      <div className="flex flex-wrap items-center justify-between gap-2 text-xs text-muted-foreground">
        <p>{jobs && tr(`${filtered.length} of ${jobs.length} jobs`, `匹配 ${filtered.length} / ${jobs.length} 个任务`)}{jobs && maxJobs != null && ` · ${tr('Storage', '记录容量')} ${jobs.length}/${maxJobs}`}</p>
        {!archived && <Button variant="outline" size="sm" disabled={!archivable.length || archivePending} onClick={() => onArchivePage(archivable)}>{archivePending ? tr('Archiving…', '正在归档…') : tr(`Archive resolved on this page (${archivable.length})`, `归档本页已结束任务（${archivable.length}）`)}</Button>}
      </div>
      {jobs && maxJobs != null && jobs.length >= maxJobs && <p role="status" className="text-sm text-destructive">{archived ? tr('Archive storage is full. Records are not automatically deleted.', '归档记录已满；系统不会自动删除记录。') : tr('Job storage is full. Archive resolved jobs to allow new jobs and automatic recovery.', '未归档任务已满；请归档已结束的任务，否则可能无法创建新任务或自动恢复。')}</p>}
      {!archived && <p className="text-xs text-muted-foreground">{tr('The list refreshes automatically. Archiving does not turn off automatic recovery. Local import retries do not authorize again.', '列表自动更新。归档不影响账号自动恢复；重试本地入库不会重新授权。')}</p>}
      {(error || actionError('archive-page')) && <p role="alert" className="break-words text-sm text-destructive">{error ? `${tr('Could not load jobs; use Refresh list:', '读取任务失败，请刷新列表：')}${error}` : actionError('archive-page')}</p>}
      {jobs === null ? <p className="text-sm text-muted-foreground">{error ? tr('Waiting for a successful retry.', '等待重试成功。') : tr('Loading jobs…', '正在读取任务…')}</p>
        : !visible.length ? <p className="text-sm text-muted-foreground">{jobs.length ? tr('No jobs match these filters.', '没有匹配当前筛选的任务。') : tr('No jobs in this view.', '此处暂无任务。')}</p>
        : <div className="divide-y divide-border rounded-md border border-border">{visible.map(job => {
          const jobBusy = !!busy[`job:${job.id}`] || archivePending
          const results = new Map(job.account_results?.map(row => [row.line, row]))
          return <article key={job.id} id={`oauth-job-${job.id}`} className="space-y-2 px-3 py-2.5 text-sm">
            <div className="grid min-w-0 gap-1 md:grid-cols-[minmax(0,1fr)_auto] md:items-center">
              <div className="flex min-w-0 flex-wrap items-center gap-x-2 gap-y-1"><span className={`rounded border px-1.5 py-0.5 text-xs ${needsReview(job.state) ? 'border-destructive/40 text-destructive' : 'border-border'}`}>{stateText(job.state)}</span><span className="font-medium">{kindOptions.find(row => row.value === jobKind(job))?.label || unknown}</span><span className="break-all text-xs text-muted-foreground">{job.id}</span></div>
              <p className="text-xs tabular-nums text-muted-foreground">{tr('Imported', '已导入')} {job.imported}/{job.total} · {tr('Failed', '失败')} {job.failed} · {tr('Pending', '待处理')} {job.pending ?? '—'}</p>
            </div>
            <div className="flex flex-wrap items-center gap-x-2 gap-y-1 text-xs text-muted-foreground">
              {job.archived && <span>{tr('Archived', '已归档')}{job.archived_at && ` ${timestamp(job.archived_at)}`}</span>}
              {job.created_at && <span>{timestamp(job.created_at)}</span>}
              {job.recovery_account_id && <span>{tr('Original account', '原账号')} {accountLink(job.recovery_account_id)}</span>}
              {job.group_ids?.map(id => <span className="rounded border border-border px-1.5 py-0.5" key={id}>{groupLabel(id)}</span>)}
            </div>
            <details open={!!expanded[job.id]} onToggle={e => { const open = e.currentTarget.open; setExpanded(old => !!old[job.id] === open ? old : { ...old, [job.id]: open }) }}>
              <summary className="cursor-pointer font-medium focus-visible:outline-2 focus-visible:outline-ring">{job.archived ? tr('Archived summary', '归档摘要') : tr('Batch and account details', '批次与账号详情')}</summary>
              <div className="mt-2 space-y-2 border-l-2 border-border pl-3">
                <p className="text-xs text-muted-foreground">{tr('Requested exit', '请求出口')}: {job.requested_country === 'auto' ? tr('Automatic', '自动') : job.requested_country || unknown} · {tr('Observed exit', '实际出口')}: {job.observed_country || unknown}</p>
                {job.retry_of && <p className="break-all">{tr('Reauthorization source', '重新授权来源')}: {job.retry_of.job_id} · {tr('Line', '第')} {job.retry_of.line}</p>}
                {!job.archived && <>
                  <div className="flex flex-wrap gap-2">
                    {job.can_refresh && <Button variant="outline" size="sm" disabled={jobBusy} onClick={() => onAction(job, 'refresh')}>{tr('Check original job', '核查原任务')}</Button>}
                    {job.can_cancel && <Button variant="outline" size="sm" disabled={jobBusy} onClick={() => onAction(job, 'cancel')}>{tr('Cancel job', '取消任务')}</Button>}
                    {job.can_retry_import && <Button variant="outline" size="sm" disabled={jobBusy} onClick={() => onAction(job, 'retry-import')}>{tr('Retry local import (no new authorization)', '重试本地入库（不重新授权）')}</Button>}
                    {job.can_archive && <Button variant="outline" size="sm" disabled={jobBusy} onClick={() => onAction(job, 'archive')}>{tr('Archive (keep recovery)', '归档（保留自动恢复）')}</Button>}
                  </div>
                  {needsReview(job.state) && <p className="text-xs text-muted-foreground">{tr('Check the original outcome before retrying. An unknown result is not a failed authorization.', '请先核查原任务；结果未知不等于授权失败，不能盲目重试。')}</p>}
                  {actionError(`job:${job.id}`) && <p role="alert" className="break-words text-destructive">{actionError(`job:${job.id}`)}</p>}
                </>}
                {job.groups?.map(batch => <div key={batch.index} className="space-y-1 border-t border-border pt-2">
                  <p>{tr('Batch', '批次')} {batch.index + 1}: {stateText(batch.state)} · {batch.imported}/{batch.total}</p>
                  <p className="break-all text-xs text-muted-foreground">{tr('Batch ID', '批次 ID')}: {batch.batch_id || '—'} · {tr('Request', '请求')}: {batch.request_id || '—'}</p>
                  {batch.error && <p className="break-words text-destructive">{problemText(batch.error)}</p>}
                  {(batch.read_failures || batch.next_check_at) && <p>{tr('Read / failures / next check', '读取 / 失败次数 / 下次检查')}: {batch.read_operation || '—'} / {batch.read_failures || 0} / {timestamp(batch.next_check_at) || '—'}</p>}
                  {batch.items?.map(item => {
                    const result = results.get(item.line)
                    return <div key={item.line} className="break-words border-t border-border/60 py-2 pl-2">
                      <p>{item.line}. <span className="break-all">{item.identifier || item.identifier_masked || '—'}</span> · OAuth: {stateText(item.state)} · {tr('Import', '导入')}: {item.state === 'failed' && item.import_state === 'not_ready' ? tr('Not imported (authorization failed)', '未导入（授权失败）') : stateText(item.import_state)}{result?.account_id && <> · {accountLink(result.account_id)}</>}</p>
                      {result ? resultDetails(result) : item.import_state === 'imported' && resultDetails({ line: item.line })}
                      {item.error && <p className="text-destructive">{problemText(item.error)}</p>}
                      {item.diagnostic && <p>{item.diagnostic.upstream_code === 'invalid_state' && <span>{tr('Login session has expired.', '登录会话已失效。')} </span>}{[item.diagnostic.upstream_code && `${tr('Cause', '失败原因')}: ${item.diagnostic.upstream_code}`, item.diagnostic.http_status && `HTTP ${item.diagnostic.http_status}`, item.diagnostic.stage && `${tr('Stage', '阶段')}: ${item.diagnostic.stage === 'totp_verify' ? tr('TOTP verification', 'TOTP 验证') : item.diagnostic.stage}`, item.diagnostic.endpoint && `${tr('Endpoint', '接口')}: ${item.diagnostic.endpoint}`].filter(Boolean).join(' · ')}</p>}
                      {item.can_reauthorize && <Button variant="outline" size="sm" disabled={jobBusy || reauthorizeDisabled || !item.identifier || !Number.isSafeInteger(item.line) || item.line < 1} onClick={() => onReauthorize(job, item)}>{tr('Reauthorize this account (new job)', '重新授权此账号（新任务）')}</Button>}
                    </div>
                  })}
                </div>)}
                {!job.groups?.length && (job.account_results?.length ? job.account_results.map(row => <div className="border-t border-border pt-2" key={row.line}><p>{row.line}. {row.identifier_masked || '—'}{row.account_id && <> · {accountLink(row.account_id)}</>}{row.outcome && ` · ${stateText(row.outcome)}`}</p>{row.account_id && resultDetails(row)}</div>) : <p className="text-xs text-muted-foreground">{job.archived ? tr('This archive has no per-account snapshot.', '该归档没有保留逐账号结果。') : tr('No batch details yet.', '暂无批次详情。')}</p>)}
              </div>
            </details>
          </article>
        })}</div>}
      {jobs && <nav aria-label={tr('Job pagination', '任务分页')} className="flex flex-wrap items-center justify-between gap-2">
        <Select aria-label={tr('Jobs per page', '每页任务数')} className="w-auto min-w-28" compact value={String(pageSize)} options={[10, 20, 50].map(n => ({ value: String(n), label: tr(`${n} per page`, `每页 ${n} 条`) }))} onValueChange={value => { setPageSize(Number(value)); setPage(1) }} />
        <div className="flex items-center gap-2 text-xs tabular-nums"><Button variant="outline" size="sm" disabled={current <= 1} onClick={() => setPage(current - 1)}>{tr('Previous page', '上一页')}</Button><span aria-live="polite">{tr(`Page ${current} / ${pages}`, `第 ${current} / ${pages} 页`)}</span><Button variant="outline" size="sm" disabled={current >= pages} onClick={() => setPage(current + 1)}>{tr('Next page', '下一页')}</Button></div>
      </nav>}
    </CardContent>
  </Card>
}
