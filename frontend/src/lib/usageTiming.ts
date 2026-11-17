type UsageTimingLog = {
  status_code: number
  output_tokens?: number
  completion_tokens?: number
  upstream_round_duration_ms?: number
}

const MS_PER_SECOND = 1_000

export type TimingTone = 'good' | 'warn' | 'bad' | 'muted'

export function hasMeasuredDuration(ms: unknown): ms is number {
  return typeof ms === 'number' && Number.isFinite(ms) && ms > 0
}

export function computeOutputTokensPerSec(log: UsageTimingLog): number | null {
  if (!Number.isFinite(log.status_code) || log.status_code < 200 || log.status_code >= 300) return null
  const outputTokens = log.output_tokens == null || log.output_tokens === 0
    ? log.completion_tokens
    : log.output_tokens
  const durationMs = log.upstream_round_duration_ms
  if (typeof outputTokens !== 'number' || !Number.isFinite(outputTokens) || outputTokens <= 0 ||
      !hasMeasuredDuration(durationMs)) return null
  const rate = outputTokens / durationMs * MS_PER_SECOND
  return Number.isFinite(rate) && rate > 0 ? rate : null
}

export function formatTokensPerSec(value: number): string {
  if (!Number.isFinite(value) || value <= 0) return '-'
  if (value >= 1000) return `${(value / 1000).toFixed(1)}k`
  if (value >= 100) return value.toFixed(0)
  return value.toFixed(2)
}

export function formatLatencyMs(ms: number): string {
  if (!Number.isFinite(ms) || ms <= 0) return '-'
  if (ms >= MS_PER_SECOND) return `${(ms / MS_PER_SECOND).toFixed(1)}s`
  return `${Math.round(ms)}ms`
}

export function firstTokenTone(ms: number): TimingTone {
  if (ms <= 0) return 'muted'
  if (ms > 5000) return 'bad'
  if (ms > 2000) return 'warn'
  return 'good'
}

export function durationTone(ms: number): TimingTone {
  if (ms <= 0) return 'muted'
  if (ms > 30000) return 'bad'
  if (ms > 10000) return 'warn'
  return 'good'
}
