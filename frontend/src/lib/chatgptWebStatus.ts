import type { ChatGPTWebState } from '../api'

export type WebAccountStatus = 'unknown' | 'disabled' | 'unverified' | 'ready' | 'quota_exhausted' | 'auth_required' | 'challenge'

export function webAccountStatus(state?: ChatGPTWebState | null): WebAccountStatus {
  if (state === undefined) return 'unknown'
  if (!state?.enabled) return 'disabled'
  if (state.status === 'auth_required' || state.status === 'challenge' || state.status === 'quota_exhausted') return state.status
  if (state.status === 'ready') return state.observed_at ? 'ready' : 'unverified'
  return state.status === 'unverified' ? 'unverified' : 'unknown'
}

export function webAccountStatusMessage(state?: ChatGPTWebState | null): string {
  const keys: Record<WebAccountStatus, string> = {
    unknown: 'unknown', disabled: 'statusDisabled', unverified: 'statusUnverified', ready: 'statusReady',
    quota_exhausted: 'statusQuota', auth_required: 'statusAuth', challenge: 'statusChallenge',
  }
  return `chatgptWeb.${keys[webAccountStatus(state)]}`
}

export function webImageQuotaRemaining(state?: ChatGPTWebState | null): number | null {
  const value = state?.image_quota_remaining
  return typeof value === 'number' && Number.isSafeInteger(value) && value >= 0 ? value : null
}
