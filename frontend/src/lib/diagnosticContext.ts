export type DiagnosticRecord = Record<string, unknown>

export type DiagnosticContextParseResult =
  | { state: 'legacy' }
  | { state: 'oversize'; bytes: number; maxBytes: number }
  | { state: 'invalid'; reason: 'invalid_json' | 'unexpected_shape' }
  | { state: 'valid'; event: DiagnosticRecord; raw: string; bytes: number }

export type DiagnosticRequestScopes = {
  /** The request accepted by the gateway, before outbound adaptation. */
  ingress?: DiagnosticRecord
  /** The request shape actually submitted on this upstream attempt. */
  outbound?: DiagnosticRecord
  requestCapture?: string
}

export const DIAGNOSTIC_CONTEXT_MAX_BYTES = 24 * 1024

export function isDiagnosticRecord(value: unknown): value is DiagnosticRecord {
  return typeof value === 'object' && value !== null && !Array.isArray(value)
}

export function diagnosticString(value: unknown): string | undefined {
  return typeof value === 'string' && value.trim() ? value.trim() : undefined
}

export function diagnosticNumber(value: unknown): number | undefined {
  return typeof value === 'number' && Number.isFinite(value) ? value : undefined
}

export function diagnosticBoolean(value: unknown): boolean | undefined {
  return typeof value === 'boolean' ? value : undefined
}

export function diagnosticRecord(value: unknown): DiagnosticRecord | undefined {
  return isDiagnosticRecord(value) ? value : undefined
}

export function diagnosticArray(value: unknown): unknown[] {
  return Array.isArray(value) ? value : []
}

export function diagnosticStringArray(value: unknown): string[] {
  return diagnosticArray(value).filter((item): item is string => typeof item === 'string')
}

export function diagnosticByteLength(value: string): number {
  if (typeof TextEncoder !== 'undefined') {
    return new TextEncoder().encode(value).byteLength
  }
  return unescape(encodeURIComponent(value)).length
}

/**
 * The backend persists a bounded JSON string. Treat it as untrusted at the UI
 * boundary anyway: old rows have no value, deployment mismatches can return
 * malformed text, and a corrupt oversized value must not destabilize the page.
 */
export function parseDiagnosticContext(
  value?: string | null,
  maxBytes = DIAGNOSTIC_CONTEXT_MAX_BYTES,
): DiagnosticContextParseResult {
  if (typeof value !== 'string' || !value.trim()) {
    return { state: 'legacy' }
  }

  const bytes = diagnosticByteLength(value)
  if (bytes > maxBytes) {
    return { state: 'oversize', bytes, maxBytes }
  }

  try {
    const parsed: unknown = JSON.parse(value)
    if (!isDiagnosticRecord(parsed)) {
      return { state: 'invalid', reason: 'unexpected_shape' }
    }
    return { state: 'valid', event: parsed, raw: value, bytes }
  } catch {
    return { state: 'invalid', reason: 'invalid_json' }
  }
}

/** Keep ingress and outbound shapes separate: request rewriting/compression can make them differ. */
export function getDiagnosticRequestScopes(event: DiagnosticRecord): DiagnosticRequestScopes {
  const attempt = diagnosticRecord(event.attempt)
  return {
    ingress: diagnosticRecord(event.request),
    outbound: diagnosticRecord(attempt?.request),
    requestCapture: diagnosticString(attempt?.request_capture),
  }
}

/** The finalized event can include local/fallback evidence absent from the attempt. */
export function getDiagnosticError(event: DiagnosticRecord): DiagnosticRecord | undefined {
  return diagnosticRecord(event.error) ?? diagnosticRecord(diagnosticRecord(event.attempt)?.error)
}

/** Explanations are deliberately evidence-limited and never infer a root cause from a generic status. */
export function diagnosticErrorNotes(error?: DiagnosticRecord): string[] {
  if (!error) return ['未记录结构化错误证据；这不表示上游一定没有提供任何内容。']

  const notes: string[] = []
  const source = diagnosticString(error.source)
  const origin = source === 'downstream_response' || source === 'websocket_gateway' ? '网关' : source === 'usage_fallback' ? '用量记录' : '上游'
  const detail = diagnosticString(error.detail)
  const messagePresent = diagnosticBoolean(error.message_present)
  const messageClass = diagnosticString(error.message_class)

  switch (detail) {
    case 'generic':
      notes.push(`${origin}错误说明为通用文本；具体根因当前不可得，不能仅凭 HTTP 状态码判断。`)
      break
    case 'empty':
      notes.push(`${origin}错误响应体为空；没有可供解析的正文。`)
      break
    case 'unavailable':
      notes.push('没有可用的错误详情；这不表示上游一定没有提供任何内容。')
      break
    case 'size_limit':
      notes.push('错误详情受大小限制，仅保留可安全保存的元数据。')
      break
    case 'unstructured_omitted':
      notes.push(`${origin}返回了非结构化错误内容，原文已省略以避免保存不安全内容。`)
      break
    case 'invalid':
      notes.push('错误详情无法按预期结构解析；请结合安全证据和请求形状排查。')
      break
  }

  if (messagePresent && messageClass === 'message_omitted') {
    notes.push(`${origin}曾提供错误消息，但原始消息已因隐私保护省略；这不表示${origin}没有提供消息。`)
  }
  if (diagnosticBoolean(error.code_redacted)) {
    notes.push('上游错误 code 已被脱敏（withheld），不是未提供。')
  }
  if (diagnosticBoolean(error.type_redacted)) {
    notes.push('上游错误 type 已被脱敏（withheld），不是未提供。')
  }
  if (diagnosticBoolean(error.param_redacted)) {
    notes.push('上游错误 param 已被脱敏（withheld），不是未提供。')
  }

  return notes.length > 0 ? notes : ['已记录安全错误元数据；未从该记录推断未保存的上游内容。']
}

export function diagnosticCaptureExplanation(captureStatus?: string): string | undefined {
  switch (captureStatus) {
    case 'partial':
      return '诊断为部分采集；请查看具体缺失、裁剪或不可用标记。'
    case 'adapter_input':
      return '此处仅记录适配器输入，未确认它与最终发送帧一致。'
    case 'body_unavailable':
      return '无法取得独立的请求体读取器，未采集实际出站结构。'
    case 'unsupported_encoding':
      return '请求编码暂不支持结构检查；不代表请求 JSON 无效。'
    case 'decode_error':
      return '诊断读取器无法解码传输内容；不能据此断言客户端 JSON 无效。'
    case 'read_error':
      return '诊断读取未完成，请求结构证据不完整。'
    case 'unavailable':
      return '诊断采集不可用；无法从此记录重建原始请求。'
    case 'size_limit':
      return '诊断因大小限制被截断或省略；仅显示已保存的安全元数据。'
    case 'unstructured_omitted':
      return '非结构化上游内容已省略；这不表示上游没有返回内容。'
    case 'invalid':
      return '已保存的诊断上下文不完整或无效，部分字段可能不可用。'
    default:
      return undefined
  }
}

const candidateLabels: Record<string, string> = {
  group_mismatch: '不在有效分组', channel_mismatch: '渠道不匹配', plan_mismatch: '套餐不匹配',
  account_key_denied: '未授权给此 Key', unauthorized: '鉴权不可用', account_error: '账号异常',
  account_disabled: '账号已停用', dispatch_paused: '已暂停调度', intelligence_paused: '降智保护暂停',
  credential_missing: '缺少凭据', quota_exhausted: '额度耗尽', cooldown: '冷却中',
  egress_unavailable: '出口不可用', concurrency_saturated: '并发已满', request_filter: '请求条件排除',
  model_not_supported: '不支持实际选路模型',
  retry_excluded: '本次重试排除', available: '通过当前检查', unclassified: '未能细分',
}

export function candidateReasonLabel(reason: string): string {
  return Object.prototype.hasOwnProperty.call(candidateLabels, reason) ? candidateLabels[reason] : '未能细分'
}

const dispatchReasons: Record<string, [label: string, action: string]> = {
  no_available_account: ['没有符合条件的可用账号', '结合候选计数，检查账号鉴权、调度状态和账号侧 Key 授权。'],
  queue_full: ['等待队列已满', '检查队列容量和当前负载。'],
  queue_timeout: ['等待可用账号超时', '检查队列等待时间与可用账号并发。'],
  queue_rejected: ['请求被队列拒绝', '检查调度队列状态和准入限制。'],
  quota_exhausted: ['额度耗尽', '检查账号和 Key 的额度以及恢复时间。'],
  scope_budget: ['作用域配额受限', '检查 Key 在有效分组或渠道的配额。'],
  concurrency_saturated: ['可用账号并发已满', '检查账号并发上限和当前负载。'],
  model_not_supported: ['找不到支持实际选路模型的账号', '核对请求模型、实际选路模型和 Key 的本地目录。'],
  response_context_unavailable: ['响应上下文不可用', '检查关联请求记录与续接上下文。'],
  pinned_account_unavailable: ['续接指定的账号不可用', '检查指定账号的状态、授权和路由。'],
  proxy_unavailable: ['代理出口不可用', '检查账号所用代理的状态与连通性。'],
  upstream_pool_exhausted: ['上游候选已用尽', '按请求 ID 查看已记录的尝试，核对上游 HTTP、连接或流错误。'],
}

export function diagnosticIDs(value: unknown): number[] {
  return diagnosticArray(value).filter((id): id is number => typeof id === 'number' && Number.isSafeInteger(id) && id > 0)
}

export function candidateCounts(value: unknown): { code: string; label: string; count: number }[] {
  return Object.entries(diagnosticRecord(value) ?? {}).flatMap(([code, count]) =>
    typeof count === 'number' && Number.isSafeInteger(count) && count > 0
      ? [{ code, label: candidateReasonLabel(code), count }]
      : [],
  ).sort((a, b) => b.count - a.count)
}

export function candidateSamples(value: unknown): { id: number; reason: string }[] {
  return diagnosticArray(value).flatMap((item) => {
    const sample = diagnosticRecord(item)
    const [id] = diagnosticIDs([sample?.account_id])
    const reason = diagnosticString(sample?.reason)
    return id !== undefined && reason ? [{ id, reason }] : []
  }).sort((a, b) => Number(b.reason === 'account_key_denied') - Number(a.reason === 'account_key_denied'))
}

export function getDispatchDecision(event: DiagnosticRecord) {
  const decision = diagnosticRecord(event.decision)
  const reason = diagnosticString(decision?.reason)
  if (!decision || !reason) return undefined
  const [label, action] = Object.prototype.hasOwnProperty.call(dispatchReasons, reason) ? dispatchReasons[reason] : ['未能细分调度原因', '查看已保存的候选和尝试记录。']
  const attempted = diagnosticBoolean(decision.attempted)
  const counts = candidateCounts(decision.counts)
  const dispatch = attempted === false ? '未派发' : attempted === true ? '已尝试上游' : '未记录是否派发'
  const blockers = counts.filter(({ code }) => code !== 'available')
  const inScope = blockers.filter(({ code }) => !['group_mismatch', 'channel_mismatch', 'plan_mismatch'].includes(code))
  const highlights = inScope.length > 0 ? inScope : blockers
  return {
    decision, attempted, counts, action, dispatch, label,
    summary: reason === 'no_available_account' && highlights.length > 0
      ? highlights.slice(0, 2).map(({ count, label }) => `${count} 个账号${label}`).join('，')
      : label,
  }
}

export function usageDiagnosticStatus(log: { status_code: number; diagnostic_context?: string | null; error_message?: string; upstream_error_kind?: string }) {
  const parsed = parseDiagnosticContext(log.diagnostic_context)
  const event = parsed.state === 'valid' ? parsed.event : undefined
  const outcomeStatus = diagnosticNumber(event?.outcome_status)
  const failed = log.status_code >= 400 || (outcomeStatus ?? 0) >= 400
    || event?.outcome === 'failure' || event?.outcome === 'error'
    || Boolean(log.error_message?.trim() || log.upstream_error_kind?.trim())
  if (!failed) return { failed: false as const }
  const failure = (label: string) => ({ failed: true as const, label, httpStatus: diagnosticNumber(event?.downstream_http_status) ?? log.status_code })
  if (!event) return failure(parsed.state === 'legacy' ? '历史未采集' : '诊断详情不可读')
  const error = getDiagnosticError(event)
  const stage = diagnosticString(event.stage)
  const source = diagnosticString(error?.source)
  const upstream = stage?.startsWith('upstream') || source?.startsWith('upstream')
  const decision = getDispatchDecision(event)
  if (decision) return failure(`${decision.dispatch} · ${decision.decision.reason === 'no_available_account' && decision.counts.some(({ code }) => code === 'account_key_denied') ? '无符合授权的可用账号' : decision.label}`)
  if (upstream) return failure(error?.detail === 'generic' ? '上游未提供可定位原因' : '上游请求失败')
  if (stage === 'dispatch') return failure('调度原因未采集')
  if (source === 'downstream_response' || source === 'websocket_gateway' || stage === 'local' || stage === 'local_or_gateway' || stage === 'websocket_local_or_gateway' || stage === 'authentication') {
    return failure('网关返回错误')
  }
  return failure('失败来源未确认')
}
