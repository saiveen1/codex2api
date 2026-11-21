import { useEffect, useState, type ReactElement } from 'react'
import { Link } from 'react-router-dom'
import { api } from '../api'
import type { APIKeyModelDiagnostics, ModelCatalogDiagnostic } from '../types'
import { candidateCounts, candidateReasonLabel, candidateSamples, diagnosticIDs, diagnosticRecord } from '../lib/diagnosticContext'
import { formatBeijingTime } from '../utils/time'
import { Button } from '@/components/ui/button'
import { Dialog, DialogContent, DialogDescription, DialogHeader, DialogTitle, DialogTrigger } from '@/components/ui/dialog'

const catalogReasons: Record<string, [label: string, action: string]> = {
  no_scoped_accounts: ['作用域内没有账号', '检查 Key 的有效分组与渠道是否包含目标账号。'],
  no_visible_accounts: ['作用域内没有目录可见的账号', '检查下方排除原因，并核对账号状态、套餐和账号侧 Key 授权。'],
  no_account_models: ['可见账号未产出本地模型', '检查本地模型来源；这不表示官方上游一定不支持该模型。'],
  models_filtered: ['模型被 Key 的规则全部过滤', '检查此 Key 的模型允许和拒绝规则。'],
  ok: ['本地目录有可见模型', '如果请求仍失败，请查看该请求当时的调度诊断。'],
  uncollected: ['未采集目录证据', '重新检查本地目录，或在客户端再次查询后查看最近空目录记录。'],
}

export function CandidateEvidence({ counts, samples }: { counts: unknown; samples: unknown }) {
  const rows = candidateCounts(counts)
  const accounts = candidateSamples(samples)
  return <div className="space-y-3">
    {rows.length ? <dl className="grid gap-x-5 gap-y-1.5 sm:grid-cols-2">
      {rows.map(({ code, label, count }) => <div key={code} className="flex justify-between gap-2 border-b border-border py-1">
        <dt className="min-w-0 break-words text-muted-foreground">{label}</dt><dd className="shrink-0 font-mono tabular-nums">{count}</dd>
      </div>)}
    </dl> : <p className="text-muted-foreground">未记录分原因计数。</p>}
    {accounts.length ? <ul aria-label="账号样本" className="flex flex-wrap gap-2">
      {accounts.map(({ id, reason }) => <li key={`${id}-${reason}`}>
        <Link to={`/accounts?account_id=${id}`} className="inline-flex min-h-8 max-w-full items-center break-words rounded-lg border border-border px-2 py-1 text-primary underline-offset-4 hover:bg-accent hover:underline focus-visible:outline-2 focus-visible:outline-ring">
          账号 #{id}：{candidateReasonLabel(reason)}
        </Link>
      </li>)}
    </ul> : null}
  </div>
}

function CatalogResult({ value }: { value: ModelCatalogDiagnostic }) {
  const [label, action] = Object.prototype.hasOwnProperty.call(catalogReasons, value.reason) ? catalogReasons[value.reason] : ['目录原因未能细分', '重新检查本地目录。']
  const groups = diagnosticIDs(value.group_ids)
  return <div className="space-y-3 text-sm">
    <div>
      <p className="break-words font-semibold text-foreground">{label}</p>
      <p className="mt-1 text-muted-foreground">{action}</p>
    </div>
    <dl className="grid gap-2 text-xs sm:grid-cols-2">
      <div><dt className="text-muted-foreground">有效分组</dt><dd className="break-words">{groups.length ? groups.join('、') : '未列出分组'}</dd></div>
      <div><dt className="text-muted-foreground">渠道</dt><dd className="break-words">{value.channel || '未列出渠道'}</dd></div>
      <div><dt className="text-muted-foreground">作用域内账号</dt><dd>{value.scoped ?? '未记录'}</dd></div>
      <div><dt className="text-muted-foreground">目录可见账号</dt><dd>{value.visible ?? '未记录'}</dd></div>
      {value.time ? <div><dt className="text-muted-foreground">采集时间</dt><dd><time dateTime={value.time}>{formatBeijingTime(value.time)}</time></dd></div> : null}
    </dl>
    {value.reason !== 'uncollected' ? <div>
      <p className="text-xs font-medium text-foreground">最终本地模型</p>
      {value.models?.length ? <ul className="mt-1.5 flex flex-wrap gap-1.5">{value.models.map(model => <li key={model} className="max-w-full break-all rounded-md bg-muted px-2 py-1 font-mono text-xs">{model}</li>)}</ul> : <p className="mt-1 text-xs text-muted-foreground">{value.reason === 'ok' ? '未附模型清单。' : '没有可见模型。'}</p>}
    </div> : null}
    {value.counts || value.samples?.length ? <details>
      <summary className="cursor-pointer rounded text-xs font-medium focus-visible:outline-2 focus-visible:outline-ring">排除计数与账号样本</summary>
      <div className="mt-2 text-xs"><CandidateEvidence counts={value.counts} samples={value.samples} /></div>
    </details> : null}
  </div>
}

function ModelDiagnosticsContent({ keyId }: { keyId: number }) {
  const [revision, setRevision] = useState(0)
  const [state, setState] = useState<{ data?: APIKeyModelDiagnostics; failed?: boolean }>({})
  const { data, failed } = state
  const loading = !failed && !data

  useEffect(() => {
    const controller = new AbortController()
    void api.getAPIKeyModelDiagnostics(keyId, controller.signal).then(data => {
      if (controller.signal.aborted) return
      setState(diagnosticRecord(data?.current) && typeof data.current.reason === 'string' ? { data } : { failed: true })
    }, () => { if (!controller.signal.aborted) setState({ failed: true }) })
    return () => controller.abort()
  }, [keyId, revision])

  return <>
    {loading ? <p role="status" className="rounded-lg border border-border p-3 text-sm text-muted-foreground">正在检查本地目录…</p> : null}
    {failed ? <p role="alert" className="rounded-lg border border-destructive/25 bg-destructive/5 p-3 text-sm text-foreground">未能读取模型目录诊断。请重试，或确认该 Key 仍存在、服务已支持目录诊断。</p> : null}
    {data ? <>
      <section aria-label="当前检查" className="min-w-0 space-y-2 rounded-lg border border-primary/20 bg-primary/5 p-3 sm:p-4">
        <h3 className="text-sm font-semibold text-primary">当前检查</h3>
        <CatalogResult value={data.current} />
      </section>
      <section aria-label="最近空目录" className="min-w-0 space-y-2 rounded-lg border border-border bg-card p-3 sm:p-4">
        <h3 className="text-sm font-semibold">最近空目录</h3>
        {data.last_empty ? <>
          <CatalogResult value={data.last_empty} />
          {data.last_empty.request_id ? <p className="break-all text-xs text-muted-foreground">请求 ID：<code>{data.last_empty.request_id}</code></p> : null}
        </> : <p className="text-sm text-muted-foreground">{data.last_empty_status === 'expired'
          ? '最近空目录快照已过期。'
          : data.last_empty_status === 'cache_unavailable'
            ? '暂时无法读取空目录快照，当前本地检查不受影响。'
            : '没有可用的空目录快照：可能尚未采集，或已超过 24 小时保留期。'}</p>}
      </section>
    </> : null}
    <div className="flex justify-end">
      <Button type="button" size="sm" variant="outline" disabled={loading} className="transition-colors motion-reduce:transition-none" onClick={() => { setState({}); setRevision(value => value + 1) }}>{failed ? '重试检查' : '重新检查'}</Button>
    </div>
  </>
}

export function ModelDiagnosticsDialog({ keyId, open, onOpenChange, trigger }: {
  keyId: number; open?: boolean; onOpenChange?: (open: boolean) => void; trigger?: ReactElement
}) {
  return <Dialog open={open} onOpenChange={onOpenChange}>
    {trigger ? <DialogTrigger asChild>{trigger}</DialogTrigger> : null}
    <DialogContent className="grid-cols-[minmax(0,1fr)] motion-reduce:animate-none sm:max-w-2xl">
      <DialogHeader className="pr-5 text-left">
        <DialogTitle>检查模型目录</DialogTitle>
        <DialogDescription>Key #{keyId} · 只检查本地目录，不调用上游、不发送推理。实际由上游补全的 Codex manifest 不在此检查范围内。</DialogDescription>
      </DialogHeader>
      <ModelDiagnosticsContent key={keyId} keyId={keyId} />
    </DialogContent>
  </Dialog>
}
