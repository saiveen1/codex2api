import { useMemo, useState, type ReactElement, type ReactNode } from 'react'
import { Link } from 'react-router-dom'
import { CandidateEvidence } from './ModelDiagnosticsDialog'
import { Check, ChevronDown, Copy, Fingerprint, Info, Network, ShieldAlert } from 'lucide-react'
import type { UsageLog } from '../types'
import {
  diagnosticArray,
  diagnosticBoolean,
  diagnosticCaptureExplanation,
  diagnosticErrorNotes,
  diagnosticIDs,
  diagnosticNumber,
  diagnosticRecord,
  diagnosticString,
  diagnosticStringArray,
  getDiagnosticError,
  getDiagnosticRequestScopes,
  getDispatchDecision,
  usageDiagnosticStatus,
  parseDiagnosticContext,
  type DiagnosticRecord,
} from '../lib/diagnosticContext'
import { Button } from '@/components/ui/button'
import { Badge } from '@/components/ui/badge'
import {
  Dialog,
  DialogContent,
  DialogDescription,
  DialogHeader,
  DialogTitle,
  DialogTrigger,
} from '@/components/ui/dialog'

const enumLabels: Record<string, Record<string, string>> = {
  capture_status: {
    complete: '已采集 / complete',
    partial: '部分采集 / partial',
    captured: '已采集 / captured',
    unavailable: '不可用 / unavailable',
    size_limit: '大小限制 / size limit',
    unstructured_omitted: '非结构化内容已省略 / unstructured omitted',
    invalid: '无效 / invalid',
  },
  request_capture: {
    decoded_transport: '从实际传输字节解析 / decoded transport',
    websocket_frame: '最终 WebSocket 帧 / final frame',
    adapter_input: '仅适配器输入 / adapter input',
    body_unavailable: '请求体不可读 / body unavailable',
    unsupported_encoding: '不支持的编码 / unsupported encoding',
    decode_error: '解码失败 / decode error',
    read_error: '读取失败 / read error',
    captured: '已采集 / captured',
    unavailable: '不可用 / unavailable',
    size_limit: '大小限制 / size limit',
    unstructured_omitted: '非结构化内容已省略 / unstructured omitted',
    invalid: '无效 / invalid',
  },
  stage: {
    upstream_http: '上游 HTTP 拒绝 / upstream HTTP',
    upstream_stream: '流内失败 / upstream stream',
    upstream_transport: '连接失败 / upstream transport',
    local: '本地拒绝 / local',
    local_or_gateway: '本地或网关 / local or gateway',
    websocket_local_or_gateway: 'WebSocket 网关错误 / WS gateway',
    authentication: '鉴权失败 / authentication',
    completion: '请求结束 / completion',
    continuation: '续想 / continuation',
    panic: '内部异常 / panic',
    ingress: '入站 / ingress',
    upstream: '上游 / upstream',
    dispatch: '调度 / dispatch',
    retry: '重试 / retry',
    transport: '传输 / transport',
    response: '响应 / response',
  },
  transport: {
    timeout: '超时 / timeout',
    canceled: '已取消 / canceled',
    dns: 'DNS 失败 / DNS',
    tls: 'TLS 失败 / TLS',
    transport: '连接错误 / transport',
    eof: '意外断流 / EOF',
    http: 'HTTP / http',
    websocket: 'WebSocket / websocket',
    ws: 'WebSocket / ws',
  },
  protocol: {
    http: 'HTTP / http',
    https: 'HTTPS / https',
    websocket: 'WebSocket / websocket',
    ws: 'WebSocket / ws',
  },
  outcome: {
    failure: '失败 / failure',
    unknown: '结果未知 / unknown',
    error: '失败 / error',
    success: '成功 / success',
    retry: '重试 / retry',
    canceled: '已取消 / canceled',
  },
}

function formatEnum(group: keyof typeof enumLabels, value: unknown): string {
  const raw = diagnosticString(value)
  if (!raw) return '—'
  return enumLabels[group][raw] ?? `未知值 / unknown: ${raw}`
}

function text(value: unknown, fallback = '—'): string {
  if (typeof value === 'string') return value || fallback
  if (typeof value === 'number' || typeof value === 'boolean') return String(value)
  return fallback
}

function formatBytes(value: unknown): string {
  const bytes = diagnosticNumber(value)
  if (bytes === undefined || bytes < 0) return '—'
  if (bytes < 1024) return `${bytes} B`
  if (bytes < 1024 * 1024) return `${(bytes / 1024).toFixed(1)} KiB`
  return `${(bytes / 1024 / 1024).toFixed(2)} MiB`
}

function formatMilliseconds(value: unknown): string {
  const ms = diagnosticNumber(value)
  if (ms === undefined) return '—'
  return ms >= 1000 ? `${(ms / 1000).toFixed(2)} s` : `${ms} ms`
}

function joinValues(values: string[]): string {
  return values.length > 0 ? values.join(', ') : '—'
}

function asRecordList(value: unknown): DiagnosticRecord[] {
  return diagnosticArray(value).filter((item): item is DiagnosticRecord => diagnosticRecord(item) !== undefined)
}

function summaryFromEvent(event: DiagnosticRecord): string | undefined {
  const dispatch = getDispatchDecision(event)
  if (dispatch) return `${dispatch.dispatch}：${dispatch.summary}`
  const summary = diagnosticString(event.summary)
  if (summary) return summary
  const error = getDiagnosticError(event)
  const detail = diagnosticString(error?.detail)
  if (detail === 'generic') return '上游返回了通用错误说明，具体根因未保存。'
  if (detail === 'size_limit') return '错误详情受大小限制，仅保留安全元数据。'
  if (detail === 'unstructured_omitted') return '上游非结构化错误内容已省略，保留了安全元数据。'
  if (detail === 'unavailable') return '没有可用错误详情；不能据此判断上游未提供内容。'
  return undefined
}

export function diagnosticContextSummary(
  diagnosticContext?: string | null,
  fallback?: string | null,
): string {
  const parsed = parseDiagnosticContext(diagnosticContext)
  if (parsed.state === 'legacy') {
    const legacy = '历史记录未采集诊断上下文，无法重建原始请求。'
    return fallback?.trim() ? `${fallback.trim()} · ${legacy}` : legacy
  }
  if (parsed.state === 'oversize') {
    return '诊断上下文超过前端安全读取限制，未展开。'
  }
  if (parsed.state === 'invalid') {
    return '诊断上下文格式无效，无法安全解析。'
  }
  return summaryFromEvent(parsed.event) || fallback?.trim() || '已记录安全诊断元数据；未保存原始请求内容。'
}

function DetailRow({ label, value, mono = false }: { label: string; value: ReactNode; mono?: boolean }) {
  return (
    <div className="grid min-w-0 gap-x-3 gap-y-0.5 text-xs sm:grid-cols-[minmax(0,0.9fr)_minmax(0,1.6fr)]">
      <span className="min-w-0 break-words text-muted-foreground">{label}</span>
      <span className={`min-w-0 [overflow-wrap:anywhere] text-foreground ${mono ? 'font-geist-mono text-[11px]' : ''}`}>{value}</span>
    </div>
  )
}

function DetailSection({ title, description, children, defaultOpen = true }: {
  title: string
  description?: string
  children: ReactNode
  defaultOpen?: boolean
}) {
  return (
    <details open={defaultOpen} className="group rounded-lg border border-border bg-card/60">
      <summary className="flex cursor-pointer list-none items-center justify-between gap-3 rounded-lg px-3 py-2.5 text-sm font-semibold text-foreground marker:content-none focus-visible:outline-2 focus-visible:outline-ring">
        <span className="min-w-0">
          {title}
          {description ? <span className="mt-0.5 block text-[11px] font-normal leading-relaxed text-muted-foreground">{description}</span> : null}
        </span>
        <ChevronDown className="size-4 shrink-0 text-muted-foreground transition-transform group-open:rotate-180 motion-reduce:transition-none" />
      </summary>
      <div className="border-t border-border px-3 py-3">{children}</div>
    </details>
  )
}

function StatusPill({ label, value, group }: { label: string; value: unknown; group?: keyof typeof enumLabels }) {
  const display = group ? formatEnum(group, value) : text(value)
  if (display === '—') return null
  return <Badge variant="outline" className="max-w-full break-all text-[11px] font-normal">{label}: {display}</Badge>
}

function ShapeCounts({ shape }: { shape: DiagnosticRecord }) {
  const items = [
    ['请求字节 / bytes', formatBytes(shape.bytes)],
    ['检查状态 / inspection', text(shape.inspection)],
    ['输入项 / input items', text(shape.input_items)],
    ['消息 / messages', text(shape.messages)],
    ['工具 / tools', text(shape.tools)],
    ['未知字段 / unknown fields', text(shape.unknown_fields)],
    ['缺失 call ID / missing call IDs', text(shape.missing_call_ids)],
    ['加密项 / encrypted items', text(shape.encrypted_items)],
    ['previous response', text(shape.previous_response)],
    ['流式 / stream', text(shape.stream)],
    ['推理强度 / reasoning effort', text(shape.reasoning_effort)],
    ['工具选择 / tool choice', text(shape.tool_choice)],
    ['未配工具的图像选择 / image choice without tool', text(shape.image_choice_without_tool)],
  ].filter(([, value]) => value !== '—')
  const fingerprints = [
    { label: '请求指纹 / fingerprint', value: diagnosticString(shape.fingerprint) },
    { label: '形状指纹 / shape fingerprint', value: diagnosticString(shape.shape_fingerprint) },
    { label: '模型指纹 / model fingerprint', value: diagnosticString(shape.model_fingerprint) },
  ].filter((item): item is { label: string; value: string } => Boolean(item.value))

  return (
    <div className="space-y-3">
      {items.length > 0 ? <div className="grid gap-2 sm:grid-cols-2">{items.map(([label, value]) => <DetailRow key={label} label={label} value={value} mono />)}</div> : <EmptyHint text="未记录此请求形状的计数。" />}
      {diagnosticStringArray(shape.fields).length > 0 ? <DetailRow label="已知字段 / known fields" value={joinValues(diagnosticStringArray(shape.fields))} mono /> : null}
      {Object.keys(diagnosticRecord(shape.item_types) ?? {}).length > 0 ? <DetailRow label="输入项类型 / item types" value={formatCountMap(diagnosticRecord(shape.item_types)!)} mono /> : null}
      {Object.keys(diagnosticRecord(shape.tool_types) ?? {}).length > 0 ? <DetailRow label="工具类型 / tool types" value={formatCountMap(diagnosticRecord(shape.tool_types)!)} mono /> : null}
      {diagnosticNumber(shape.instructions_bytes) !== undefined ? <DetailRow label="指令字节 / instructions bytes" value={formatBytes(shape.instructions_bytes)} mono /> : null}
      {fingerprints.length > 0 ? (
        <div className="rounded-md border border-primary/15 bg-primary/5 p-2.5">
          <div className="mb-2 flex items-center gap-1.5 text-[11px] font-semibold text-muted-foreground"><Fingerprint className="size-3.5" />用于比较失败模式，不含请求正文 / comparison only, no bodies</div>
          <div className="space-y-1.5">{fingerprints.map((item) => <DetailRow key={item.label} label={item.label} value={item.value} mono />)}</div>
        </div>
      ) : null}
    </div>
  )
}

function formatCountMap(record: DiagnosticRecord): string {
  return Object.entries(record).map(([key, value]) => `${key}: ${text(value)}`).join(', ')
}

function EmptyHint({ text: message }: { text: string }) {
  return <p className="text-xs leading-relaxed text-muted-foreground">{message}</p>
}

function ParameterShapes({ shape }: { shape: DiagnosticRecord }) {
  const parameters = asRecordList(shape.parameters)
  if (parameters.length === 0) return <EmptyHint text="未记录顶层参数形状。" />
  return (
    <div className="space-y-2">
      {parameters.map((parameter, index) => (
        <div key={`${diagnosticString(parameter.path) ?? 'parameter'}-${index}`} className="rounded-md border border-border/70 bg-muted/20 p-2.5">
          <div className="flex flex-wrap items-center gap-x-2 gap-y-1 font-geist-mono text-[11px] text-foreground">
            <span>{diagnosticString(parameter.path) ?? `parameter[${index}]`}</span>
            <span className="text-muted-foreground">{diagnosticString(parameter.type) ?? 'unknown type'}</span>
            {diagnosticNumber(parameter.bytes) !== undefined ? <span className="text-muted-foreground">{formatBytes(parameter.bytes)}</span> : null}
          </div>
          {parameter.value !== undefined ? <div className="mt-1.5 break-all text-[11px] text-muted-foreground">value: {safeJson(parameter.value)}</div> : null}
          {parameter.items !== undefined ? <div className="mt-1 break-all text-[11px] text-muted-foreground">items: {safeJson(parameter.items)}</div> : null}
        </div>
      ))}
    </div>
  )
}

function SampleList({ title, samples, kind }: { title: string; samples: DiagnosticRecord[]; kind: 'item' | 'tool' }) {
  if (samples.length === 0) return <EmptyHint text={`未记录${title}。`} />
  return (
    <div className="space-y-2">
      {samples.map((sample, index) => (
        <details key={`${diagnosticString(sample.path) ?? kind}-${index}`} className="rounded-md border border-border/70 bg-muted/15 px-2.5 py-2">
          <summary className="cursor-pointer break-all font-geist-mono text-[11px] text-foreground">
            #{text(sample.index, String(index + 1))} · {diagnosticString(sample.path) ?? kind} · {diagnosticString(sample.type) ?? 'unknown'} · {formatBytes(sample.bytes)}
          </summary>
          <div className="mt-2 space-y-1.5 border-t border-border/60 pt-2">
            {kind === 'item' ? <>
              <DetailRow label="角色 / role" value={text(sample.role)} mono />
              <DetailRow label="内容类型 / content types" value={joinValues(diagnosticStringArray(sample.content_types))} mono />
              <DetailRow label="call ID 指纹" value={text(sample.call_id_fingerprint)} mono />
              <DetailRow label="名称指纹 / name fingerprint" value={text(sample.name_fingerprint)} mono />
              <DetailRow label="arguments / output / encrypted" value={`${formatBytes(sample.arguments_bytes)} / ${formatBytes(sample.output_bytes)} / ${formatBytes(sample.encrypted_bytes)}`} mono />
            </> : <>
              <DetailRow label="名称指纹 / name fingerprint" value={text(sample.name_fingerprint)} mono />
              <DetailRow label="parameters bytes" value={formatBytes(sample.parameters_bytes)} mono />
              <DetailRow label="properties / required" value={`${text(sample.properties)} / ${text(sample.required)}`} mono />
              <DetailRow label="strict" value={text(sample.strict)} mono />
            </>}
          </div>
        </details>
      ))}
    </div>
  )
}

function RequestShapeSection({ title, description, shape }: { title: string; description: string; shape?: DiagnosticRecord }) {
  if (!shape) return <DetailSection title={title} description={description} defaultOpen={false}><EmptyHint text="此记录没有该请求形状；这不代表另一侧请求不存在。" /></DetailSection>
  return (
    <DetailSection title={title} description={description} defaultOpen={false}>
      <div className="space-y-3">
        <ShapeCounts shape={shape} />
        <details className="rounded-md border border-border/70 bg-muted/10 p-2.5">
          <summary className="cursor-pointer text-xs font-semibold text-foreground">顶层参数形状 / top-level parameter shapes</summary>
          <div className="mt-2"><ParameterShapes shape={shape} /></div>
        </details>
        <details className="rounded-md border border-border/70 bg-muted/10 p-2.5">
          <summary className="cursor-pointer text-xs font-semibold text-foreground">输入样本 / ordered item samples{diagnosticBoolean(shape.samples_truncated) ? '（已截断 / truncated）' : ''}</summary>
          <div className="mt-2"><SampleList title="输入样本" samples={asRecordList(shape.item_samples)} kind="item" /></div>
        </details>
        <details className="rounded-md border border-border/70 bg-muted/10 p-2.5">
          <summary className="cursor-pointer text-xs font-semibold text-foreground">工具样本 / ordered tool samples{diagnosticBoolean(shape.samples_truncated) ? '（已截断 / truncated）' : ''}</summary>
          <div className="mt-2"><SampleList title="工具样本" samples={asRecordList(shape.tool_samples)} kind="tool" /></div>
        </details>
      </div>
    </DetailSection>
  )
}

function ErrorSection({ error }: { error?: DiagnosticRecord }) {
  const evidence = asRecordList(error?.evidence)
  const fields = [
    { label: 'detail', value: diagnosticString(error?.detail) },
    { label: 'source', value: diagnosticString(error?.source) },
    { label: 'code', value: diagnosticBoolean(error?.code_redacted) ? '已脱敏 / withheld' : diagnosticString(error?.code) },
    { label: 'type', value: diagnosticBoolean(error?.type_redacted) ? '已脱敏 / withheld' : diagnosticString(error?.type) },
    { label: 'param', value: diagnosticBoolean(error?.param_redacted) ? '已脱敏 / withheld' : diagnosticString(error?.param) },
    { label: 'message class', value: diagnosticString(error?.message_class) },
    { label: 'error fingerprint', value: diagnosticString(error?.fingerprint) },
    { label: 'body format', value: diagnosticString(error?.body_format) },
    { label: 'body bytes', value: diagnosticNumber(error?.body_bytes) !== undefined ? formatBytes(error?.body_bytes) : undefined },
    { label: 'body fingerprint', value: diagnosticString(error?.body_fingerprint) },
    { label: 'message bytes', value: diagnosticNumber(error?.message_bytes) !== undefined ? formatBytes(error?.message_bytes) : undefined },
  ].filter((item): item is { label: string; value: string } => item.value !== undefined)

  return (
    <DetailSection title="安全错误证据 / safe error evidence" description="仅显示后端已允许保存的元数据，不显示原始提示词、凭据或消息正文。" defaultOpen={false}>
      <div className="space-y-3">
        <div className="rounded-md border border-amber-500/25 bg-amber-500/5 p-2.5 text-xs leading-relaxed text-foreground">
          <div className="mb-1 flex items-center gap-1.5 font-semibold text-amber-700 dark:text-amber-300"><Info className="size-3.5" />如何理解此错误</div>
          <ul className="list-disc space-y-1 pl-4 text-muted-foreground">{diagnosticErrorNotes(error).map((note) => <li key={note}>{note}</li>)}</ul>
        </div>
        {fields.length > 0 ? <div className="grid gap-2 sm:grid-cols-2">{fields.map((item) => <DetailRow key={item.label} label={item.label} value={item.value} mono />)}</div> : <EmptyHint text="未记录结构化错误字段；不能据此断言上游没有提供任何内容。" />}
        <details className="rounded-md border border-border/70 bg-muted/10 p-2.5">
          <summary className="cursor-pointer text-xs font-semibold text-foreground">安全证据路径 / evidence{diagnosticBoolean(error?.evidence_truncated) ? '（已截断 / truncated）' : ''}</summary>
          <div className="mt-2 space-y-2">
            {evidence.length > 0 ? evidence.map((item, index) => (
              <div key={`${diagnosticString(item.path) ?? 'evidence'}-${index}`} className="rounded border border-border/60 px-2 py-1.5 font-geist-mono text-[11px] text-muted-foreground">
                {diagnosticString(item.path) ?? `evidence[${index}]`} · {diagnosticString(item.type) ?? 'unknown'} · {formatBytes(item.bytes)}{item.items !== undefined ? ` · items: ${text(item.items)}` : ''}{diagnosticString(item.fingerprint) ? ` · fp: ${diagnosticString(item.fingerprint)}` : ''}
              </div>
            )) : <EmptyHint text="未记录安全证据路径。" />}
            {diagnosticString(error?.read_error) ? <div className="text-xs text-amber-700 dark:text-amber-300">读取错误 / read error: {diagnosticString(error?.read_error)}</div> : null}
          </div>
        </details>
      </div>
    </DetailSection>
  )
}

function safeJson(value: unknown): string {
  try {
    const serialized = JSON.stringify(value)
    return serialized === undefined ? String(value) : serialized
  } catch {
    return '[unserializable]'
  }
}

async function copyText(text: string): Promise<void> {
  if (navigator.clipboard?.writeText) {
    await navigator.clipboard.writeText(text)
    return
  }
  const textarea = document.createElement('textarea')
  textarea.value = text
  textarea.readOnly = true
  textarea.style.position = 'fixed'
  textarea.style.top = '-1000px'
  document.body.appendChild(textarea)
  textarea.select()
  const copied = document.execCommand('copy')
  document.body.removeChild(textarea)
  if (!copied) throw new Error('copy failed')
}

function CopySafeJsonButton({ raw }: { raw: string }) {
  const [copied, setCopied] = useState(false)
  const handleCopy = async () => {
    try {
      await copyText(raw)
      setCopied(true)
      window.setTimeout(() => setCopied(false), 1500)
    } catch {
      setCopied(false)
    }
  }
  return (
    <Button type="button" variant="outline" size="sm" onClick={() => void handleCopy()}>
      {copied ? <Check className="size-3.5 text-emerald-600" /> : <Copy className="size-3.5" />}
      {copied ? '已复制' : '复制安全 JSON'}
    </Button>
  )
}

function DispatchOverview({ log, event }: { log: UsageLog; event: DiagnosticRecord }) {
  const result = getDispatchDecision(event)
  const [accountId] = diagnosticIDs([event.account_id, log.account_id])
  const stage = diagnosticString(event.stage)
  const error = getDiagnosticError(event)
  const status = usageDiagnosticStatus(log)
  if (!result) return <div className="space-y-1.5 rounded-lg border border-border bg-card p-3 text-sm">
    <p className="font-semibold">{status.failed ? status.label : '已记录安全诊断元数据'}</p>
    <p className="text-xs text-muted-foreground">发生阶段：{stage ? formatEnum('stage', stage) : '未采集'}。{accountId ? `关联账号 #${accountId}。` : '未记录所选账号。'}</p>
    <p className="text-xs text-muted-foreground">{error?.detail === 'generic' ? '未提供可定位的错误详情。' : summaryFromEvent(event)} 查看事件与安全证据，或按请求 ID 对照尝试记录。</p>
  </div>

  const { decision, attempted, counts, action, dispatch, label, summary } = result
  const groupIds = diagnosticIDs(decision.group_ids)
  const knownCandidate = diagnosticBoolean(decision.has_candidate)
  const partial = diagnosticBoolean(decision.partial)
  return <section aria-label="请求当时的调度结果" className="min-w-0 space-y-3 rounded-lg border border-primary/20 bg-primary/5 p-3 sm:p-4">
    <div>
      <p className="text-xs font-semibold text-primary">请求当时 · 网关调度</p>
      <p className="mt-1 break-words text-sm font-semibold">{dispatch}：{summary}</p>
      {summary !== label ? <p className="mt-1 text-xs text-muted-foreground">{label}。</p> : null}
    </div>
    <div className="grid gap-2 text-xs sm:grid-cols-2">
      <DetailRow label="所选账号" value={accountId ? <Link to={`/accounts?account_id=${accountId}`} className="rounded text-primary underline underline-offset-4 focus-visible:outline-2 focus-visible:outline-ring">账号 #{accountId}</Link> : attempted === false && knownCandidate === false ? '未选中' : '未记录'} />
      <DetailRow label="已知可用候选" value={knownCandidate === undefined ? '未记录' : knownCandidate ? '有' : partial ? '已采集范围内未发现' : '未发现'} />
      <DetailRow label="请求模型" value={diagnosticString(decision.requested_model) ?? '未记录'} mono />
      <DetailRow label="实际选路模型" value={diagnosticString(decision.effective_model) ?? '未记录'} mono />
      <DetailRow label="有效分组 / 渠道" value={`${groupIds.length ? groupIds.join('、') : '未列出分组'} / ${diagnosticString(decision.channel) ?? '未列出渠道'}`} />
      <DetailRow label="作用域内账号" value={diagnosticNumber(decision.scoped) ?? '未记录'} />
    </div>
    <p className="text-xs leading-relaxed"><strong>下一步：</strong>{action}</p>
    {partial ? <p className="text-xs text-muted-foreground">候选证据只采集了一部分，未能细分的情况不作推断。</p> : null}
    {counts.length || diagnosticArray(decision.samples).length ? <DetailSection title="候选计数与账号样本" description="诊断为有界采集，样本不是完整账号列表。" defaultOpen={false}>
      <div className="text-xs"><CandidateEvidence counts={decision.counts} samples={decision.samples} /></div>
    </DetailSection> : <EmptyHint text="此记录未保存候选计数或账号样本。" />}
  </section>
}

function DiagnosticActions({ requestId, keyId, onSearchRequest }: { requestId?: string; keyId?: number; onSearchRequest?: (id: string) => void }) {
  const [copyResult, setCopyResult] = useState('')
  return <div className="space-y-2">
    {requestId ? <p className="break-all text-xs text-muted-foreground">请求 ID：<code>{requestId}</code></p> : null}
    <div className="flex flex-wrap gap-2">
      {requestId ? <Button type="button" size="sm" variant="outline" className="transition-colors motion-reduce:transition-none" onClick={() => {
        void copyText(requestId).then(() => setCopyResult('请求 ID 已复制'), () => setCopyResult('无法复制，请手动选择上方请求 ID。'))
      }}>复制请求 ID</Button> : null}
      {requestId && onSearchRequest ? <Button type="button" size="sm" variant="outline" className="transition-colors motion-reduce:transition-none" onClick={() => onSearchRequest(requestId)}>查看同一请求的记录</Button> : null}
      {keyId && keyId > 0 ? <Button asChild size="sm" variant="outline" className="transition-colors motion-reduce:transition-none"><Link to={`/api-keys?model_diagnostics=${keyId}`}>检查此 Key 的当前模型目录</Link></Button> : null}
    </div>
    {copyResult ? <p role="status" className="text-xs text-muted-foreground">{copyResult}</p> : null}
  </div>
}

function DiagnosticDetailsContent({ log, actions }: { log: UsageLog; actions: ReactNode }) {
  const parsed = useMemo(() => parseDiagnosticContext(log.diagnostic_context), [log.diagnostic_context])
  if (parsed.state === 'legacy') {
    return <><LegacyDiagnosticState />{actions}</>
  }
  if (parsed.state === 'oversize') {
    return <><DiagnosticStateNotice title="诊断上下文过大 / context too large">该记录的诊断上下文为 {formatBytes(parsed.bytes)}，超过前端安全读取上限 {formatBytes(parsed.maxBytes)}，因此未展开。原始请求无法由此界面重建。</DiagnosticStateNotice>{actions}</>
  }
  if (parsed.state === 'invalid') {
    return <><DiagnosticStateNotice title="诊断上下文无效 / invalid context">已保存的诊断上下文不是可安全解析的 JSON（{parsed.reason === 'invalid_json' ? 'JSON 格式错误' : '顶层不是对象'}）。页面已保留原日志，不会因该记录崩溃。</DiagnosticStateNotice>{actions}</>
  }

  const event = parsed.event
  const attempt = diagnosticRecord(event.attempt)
  const error = getDiagnosticError(event)
  const scopes = getDiagnosticRequestScopes(event)
  const captureStatus = diagnosticString(event.capture_status)
  const captureExplanation = diagnosticCaptureExplanation(captureStatus)
  const attemptCaptureExplanation = diagnosticCaptureExplanation(scopes.requestCapture)
  const ids = [
    { label: 'request ID', value: diagnosticString(event.request_id) ?? log.request_id },
    { label: 'parent request ID', value: diagnosticString(event.parent_request_id) ?? log.parent_request_id },
    { label: 'upstream request ID', value: diagnosticString(event.upstream_request_id) ?? log.upstream_request_id },
    { label: 'account ID', value: text(event.account_id, log.account_id ? String(log.account_id) : '') },
    { label: 'proxy ID', value: text(event.proxy_id, log.upstream_proxy_id ? String(log.upstream_proxy_id) : '') },
  ].filter((item): item is { label: string; value: string } => Boolean(item.value))
  const overview = [
    ['schema', text(event.schema)],
    ['event', text(event.event)],
    ['stage', formatEnum('stage', event.stage)],
    ['endpoint', diagnosticString(event.endpoint) ?? log.inbound_endpoint ?? log.endpoint],
    ['kind', diagnosticString(event.kind) ?? log.upstream_error_kind],
    ['outcome', formatEnum('outcome', event.outcome)],
    ['outcome status', text(event.outcome_status)],
    ['downstream HTTP status', text(event.downstream_http_status, log.status_code ? String(log.status_code) : '—')],
    ['duration', formatMilliseconds(event.duration_ms ?? log.duration_ms)],
    ['first token', formatMilliseconds(event.first_token_ms ?? log.first_token_ms)],
  ].filter(([, value]) => value !== '—')
  const retryScheduled = diagnosticBoolean(event.retry_scheduled)
  const usageAttempt = diagnosticNumber(event.usage_attempt)

  return (
    <div className="space-y-3">
      <DispatchOverview log={log} event={event} />
      {actions}

      {(captureExplanation || attemptCaptureExplanation) ? (
        <div className="rounded-md border border-amber-500/25 bg-amber-500/5 p-2.5 text-xs leading-relaxed text-amber-800 dark:text-amber-200">
          {captureExplanation ? <div>{captureExplanation}</div> : null}
          {attemptCaptureExplanation ? <div className={captureExplanation ? 'mt-1' : ''}>实发请求采集：{attemptCaptureExplanation}</div> : null}
        </div>
      ) : null}

      <DetailSection title="事件概览与尝试记录 / event overview" description="记录的阶段、结果、重试与关联标识。" defaultOpen={false}>
        <div className="space-y-3">
          <div className="flex flex-wrap gap-1.5">
            <StatusPill label="采集 / capture" value={captureStatus} group="capture_status" />
            <StatusPill label="传输 / transport" value={attempt?.transport} group="transport" />
            <StatusPill label="实发请求采集 / request capture" value={scopes.requestCapture} group="request_capture" />
            <StatusPill label="协议 / protocol" value={attempt?.protocol} group="protocol" />
            <StatusPill label="流式 / stream" value={event.stream} />
            <StatusPill label="WebSocket" value={event.websocket} />
            {retryScheduled !== undefined ? <StatusPill label="计划重试 / retry scheduled" value={retryScheduled} /> : null}
          </div>
          <div className="grid gap-2 sm:grid-cols-2">{overview.map(([label, value]) => <DetailRow key={label} label={label} value={value} mono />)}</div>
          <div className="grid gap-2 sm:grid-cols-2">{ids.map((item) => <DetailRow key={item.label} label={item.label} value={item.value} mono />)}</div>
          <div className="grid gap-2 sm:grid-cols-2">
            <DetailRow label="尝试序号 / attempt" value={text(attempt?.index, String(log.attempt_index || '—'))} mono />
            <DetailRow label="尝试 HTTP status" value={text(attempt?.http_status)} mono />
            <DetailRow label="usage attempt" value={usageAttempt === undefined ? '—' : String(usageAttempt)} mono />
            <DetailRow label="请求线缆字节 / wire bytes" value={formatBytes(attempt?.request_bytes)} mono />
            <DetailRow label="压缩编码 / request encoding" value={text(attempt?.request_encoding)} mono />
          </div>
          <p className="text-[11px] leading-relaxed text-muted-foreground">压缩编码与线缆字节描述实际传输，不表示 JSON 是否有效；入站形状与实际提交给上游的形状分别展示。</p>
        </div>
      </DetailSection>

      <RequestShapeSection title="入站请求形状 / ingress request shape" description="网关接收到的请求；不是实际提交到上游的请求。" shape={scopes.ingress} />
      <RequestShapeSection title="实发请求形状 / submitted upstream request shape" description="此尝试实际提交给上游的形状；可能经改写、压缩或协议转换。" shape={scopes.outbound} />
      <ErrorSection error={error} />

      <details className="rounded-lg border border-border bg-muted/10">
        <summary className="cursor-pointer px-3 py-2.5 text-xs font-semibold text-foreground">安全 JSON / safe JSON disclosure</summary>
        <div className="border-t border-border px-3 py-3">
          <p className="mb-2 text-[11px] leading-relaxed text-muted-foreground">后端供应的安全元数据原样展示，未知可选字段会保留在此 JSON 中。请勿将其误解为原始提示词或凭据。</p>
          <pre className="max-h-56 overflow-auto whitespace-pre-wrap break-words rounded-md border border-border bg-background p-2.5 font-geist-mono text-[11px] leading-relaxed text-foreground">{JSON.stringify(event, null, 2)}</pre>
        </div>
      </details>
      <div className="flex justify-end"><CopySafeJsonButton raw={parsed.raw} /></div>
    </div>
  )
}

function LegacyDiagnosticState() {
  return <DiagnosticStateNotice title="历史记录未采集诊断上下文 / legacy record">此历史记录创建时未保存诊断上下文，无法重建原始请求或补回当时的上游错误内容。缺少此详情不表示上游从未提供任何内容。</DiagnosticStateNotice>
}

function DiagnosticStateNotice({ title, children }: { title: string; children: ReactNode }) {
  return (
    <div className="rounded-lg border border-amber-500/30 bg-amber-500/5 p-4">
      <div className="flex gap-2">
        <ShieldAlert className="mt-0.5 size-4 shrink-0 text-amber-600 dark:text-amber-300" />
        <div>
          <div className="font-semibold text-foreground">{title}</div>
          <div className="mt-1 text-sm leading-relaxed text-muted-foreground">{children}</div>
        </div>
      </div>
    </div>
  )
}

export function DiagnosticDetailsButton({ log, className, size = 'xs', variant = 'outline', trigger, onSearchRequest }: {
  log: UsageLog
  className?: string
  size?: 'xs' | 'sm'
  variant?: 'outline' | 'ghost'
  trigger?: ReactElement
  onSearchRequest?: (id: string) => void
}) {
  const [open, setOpen] = useState(false)
  const parsed = open ? parseDiagnosticContext(log.diagnostic_context) : undefined
  const event = parsed?.state === 'valid' ? parsed.event : undefined
  const requestId = diagnosticString(event?.request_id) ?? log.request_id
  const httpStatus = diagnosticNumber(event?.downstream_http_status) ?? log.status_code
  const outcomeStatus = diagnosticNumber(event?.outcome_status)
  return (
    <Dialog open={open} onOpenChange={setOpen}>
      <DialogTrigger asChild>
        {trigger ?? <Button type="button" variant={variant} size={size} className={className}>
          <Network className="size-3.5" />
          诊断详情
        </Button>}
      </DialogTrigger>
      <DialogContent className="grid-cols-[minmax(0,1fr)] motion-reduce:animate-none sm:max-w-3xl">
        <DialogHeader className="pr-5 text-left">
          <DialogTitle>诊断详情</DialogTitle>
          <DialogDescription>HTTP {httpStatus}{outcomeStatus && outcomeStatus !== httpStatus ? ` · 业务终态 ${outcomeStatus}` : ''} · 仅显示安全诊断元数据</DialogDescription>
        </DialogHeader>
        <DiagnosticDetailsContent log={log} actions={
          <DiagnosticActions key={requestId} requestId={requestId} keyId={log.api_key_id} onSearchRequest={onSearchRequest ? (id) => { setOpen(false); onSearchRequest(id) } : undefined} />
        } />
      </DialogContent>
    </Dialog>
  )
}
