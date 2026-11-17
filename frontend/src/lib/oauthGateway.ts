export type Problem = { code?: string; message?: string; source?: string; action?: string; retryable?: boolean; retry_after?: number; uncertain?: boolean }
export type Account = { identifier: string; password: string; totp_secret: string; proxy?: string | Record<string, unknown> }
export type GatewayInput = { source_text?: string; accounts?: Account[]; options: Record<string, string>; group_ids: number[]; save_credentials: boolean; runtime_proxy?: string; request_id?: string; recovery_account_id?: number }
export function parseMaterial(source: string, format: 'txt' | 'json'): Account[] {
  if (new TextEncoder().encode(source).length > 1048576) throw new Error('Input exceeds 1 MiB')
  const text = source.replace(/^\uFEFF/, '')
  let accounts: Account[]
  if (format === 'json') {
    let value: unknown
    try { value = JSON.parse(text) } catch { throw new Error('Invalid JSON account array') }
    if (!Array.isArray(value)) throw new Error('JSON must be an account array')
    accounts = value.map((a: unknown, i: number) => {
      if (!a || typeof a !== 'object') throw new Error(`Invalid account ${i + 1}`)
      const row = a as Record<string, unknown>
      if (Object.keys(row).some(key => !['identifier', 'password', 'totp_secret', 'proxy'].includes(key))) throw new Error(`Unknown field in account ${i + 1}`)
      const proxy = row.proxy
      if (typeof row.identifier !== 'string' || typeof row.password !== 'string' || typeof row.totp_secret !== 'string' || (proxy != null && typeof proxy !== 'string' && (typeof proxy !== 'object' || Array.isArray(proxy)))) throw new Error(`Invalid account ${i + 1}`)
      return { identifier: row.identifier, password: row.password, totp_secret: row.totp_secret, ...(proxy ? { proxy: proxy as Account['proxy'] } : {}) }
    })
  } else {
    accounts = text.split(/\r?\n/).filter(line => line.trim() !== '').map((line, i) => {
      const parts = line.split('----')
      if (parts.length !== 3) throw new Error(`Invalid line ${i + 1}; use JSON if the password contains ----`)
      return { identifier: parts[0], password: parts[1], totp_secret: parts[2] }
    })
  }
  if (!accounts.length || accounts.length > 100) throw new Error('Provide 1–100 accounts')
  return accounts
}
export function previewMatches(preview: { revision: number } | null, revision: number): boolean {
  return preview !== null && preview.revision === revision
}
export function createSubmission(input: GatewayInput, id: string): GatewayInput {
  return { ...input, request_id: id }
}

export const pendingRequestStorageKey = 'codex2api:oauth-gateway:pending-request'
// Only the opaque local request ID survives navigation; never the login material.
export function readPendingRequest(): string | null {
  try {
    const id = sessionStorage.getItem(pendingRequestStorageKey)
    return id && /^[A-Za-z0-9_.:-]{1,64}$/.test(id) && id !== '.' && id !== '..' ? id : null
  } catch { return null }
}
export function rememberPendingRequest(id: string): void {
  sessionStorage.setItem(pendingRequestStorageKey, id)
}
export function forgetPendingRequest(): void {
  // A cleanup failure must not turn a confirmed acceptance into a new submission.
  try { sessionStorage.removeItem(pendingRequestStorageKey) } catch { /* A stale ID can still be reconciled on return. */ }
}
