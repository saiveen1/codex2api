import { getAdminKey, resetAdminAuthState } from '../api'
import type { Problem } from './oauthGateway'

export class GatewayError extends Error {
  constructor(public status: number, public problem: Problem, public uncertain = false) {
    super([problem.code, problem.message, problem.action, problem.source].filter(Boolean).join(' · ') || `HTTP ${status}`)
  }
}
export async function gatewayRequest<T>(path: string, method = 'GET', body?: unknown, signal?: AbortSignal): Promise<T> {
  const createsJob = path === '/jobs' && method === 'POST'
  const controller = new AbortController()
  const abort = () => controller.abort()
  signal?.addEventListener('abort', abort, { once: true })
  if (signal?.aborted) abort()
  // The admin handler allows 30s. Bound transport/body stalls without retrying writes.
  const timeout = setTimeout(abort, 35000)
  try {
    const headers = new Headers()
    const key = getAdminKey()
    if (key) headers.set('X-Admin-Key', key)
    if (body !== undefined) headers.set('Content-Type', 'application/json')
    const response = await fetch('/api/admin/oauth-gateway' + path, { method, headers, body: body === undefined ? undefined : JSON.stringify(body), cache: 'no-store', signal: controller.signal })
    if (response.status === 401) resetAdminAuthState()
    let envelope: { ok?: boolean; data?: T; error?: Problem } | undefined
    try { envelope = await response.json() } catch (e) {
      if (controller.signal.aborted) throw e
      // Invalid or lost response bodies cannot prove whether a job was accepted.
    }
    if (!response.ok || envelope?.ok !== true) {
      const problem: Problem = envelope?.error || (response.status === 503 ? { code: 'gateway_unavailable', message: 'Gateway state unavailable. Check CODEX_CRED_ENCRYPTION_KEY and task-store ownership on the server.' } : { message: `HTTP ${response.status}` })
      throw new GatewayError(response.status, problem, createsJob && (problem.uncertain === true || response.status >= 500 || response.ok || !envelope))
    }
    if (envelope.data === undefined) throw new GatewayError(response.status, { message: 'Gateway response is missing data' }, createsJob)
    return envelope.data as T
  } catch (error) {
    if (signal?.aborted || error instanceof GatewayError) throw error
    throw new GatewayError(0, controller.signal.aborted
      ? { code: 'request_timeout', message: 'Request timed out. Check the original job before resubmitting.' }
      : { code: 'network_error', message: 'Network request failed' }, createsJob)
  } finally {
    clearTimeout(timeout)
    signal?.removeEventListener('abort', abort)
  }
}
