import { AdminAPIError, api, type ChatGPTWebState } from '../api'

interface WebFailure { code?: string; stage?: string; upstream_status?: number; recorded?: boolean; state?: ChatGPTWebState }

export function webActionError(error: unknown, fallback: string): string {
  const details = error instanceof AdminAPIError ? error.details as WebFailure | null : null
  return [fallback, error instanceof Error ? error.message : '',
    details?.code ? `code: ${details.code}` : '', details?.stage ? `stage: ${details.stage}` : '',
    details?.upstream_status ? `upstream_status: ${details.upstream_status}` : '',
  ].filter(Boolean).join(' · ')
}

export async function webFailureSnapshot(error: unknown, accountID: number): Promise<ChatGPTWebState | null | undefined> {
  const details = error instanceof AdminAPIError ? error.details as WebFailure | null : null
  if (details?.recorded === true && details.state?.account_id === accountID) return details.state
  try {
    const result = await api.getChatGPTWebAccounts()
    return result.accounts.find(item => item.account_id === accountID) ?? null
  } catch {
    // Keep both the original action error and the previous snapshot if reloading fails.
    return undefined
  }
}
