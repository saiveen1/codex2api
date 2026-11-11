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
