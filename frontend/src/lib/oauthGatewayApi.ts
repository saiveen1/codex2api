import { getAdminKey, resetAdminAuthState } from '../api'
import type { Problem } from './oauthGateway'

export class GatewayError extends Error {
  constructor(public status: number, public problem: Problem, public uncertain = false) {
    super([problem.code, problem.message, problem.action, problem.source].filter(Boolean).join(' · ') || `HTTP ${status}`)
  }
}
export async function gatewayRequest<T>(path: string, method = 'GET', body?: unknown, signal?: AbortSignal): Promise<T> {
  const createsJob = path === '/jobs' && method === 'POST'
  let response: Response
  try {
    const headers = new Headers()
    const key = getAdminKey()
    if (key) headers.set('X-Admin-Key', key)
    if (body !== undefined) headers.set('Content-Type', 'application/json')
    response = await fetch('/api/admin/oauth-gateway' + path, { method, headers, body: body === undefined ? undefined : JSON.stringify(body), cache: 'no-store', signal })
  } catch (error) {
    if (signal?.aborted) throw error
    throw new GatewayError(0, { message: 'Network request failed' }, createsJob)
  }
  if (response.status === 401) resetAdminAuthState()
  let envelope: { ok?: boolean; data?: T; error?: Problem } | undefined
  try { envelope = await response.json() } catch { /* Invalid response must not be treated as success. */ }
  if (!response.ok || envelope?.ok !== true) {
    const problem: Problem = envelope?.error || (response.status === 503 ? { code: 'gateway_unavailable', message: 'Gateway state unavailable. Check CODEX_CRED_ENCRYPTION_KEY on the server.' } : { message: `HTTP ${response.status}` })
    throw new GatewayError(response.status, problem, createsJob && (problem.uncertain === true || response.status >= 500 || response.ok || !envelope))
  }
  if (envelope.data === undefined) throw new GatewayError(response.status, { message: 'Gateway response is missing data' }, createsJob)
  return envelope.data as T
}
