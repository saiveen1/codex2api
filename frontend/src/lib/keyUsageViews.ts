export const KEY_USAGE_VIEWS = ['overview', 'quota', 'accounts', 'logs'] as const
export type KeyUsageView = typeof KEY_USAGE_VIEWS[number]

export function visibleKeyUsageViews(canViewAccounts: boolean): readonly KeyUsageView[] {
  return canViewAccounts ? KEY_USAGE_VIEWS : KEY_USAGE_VIEWS.filter(view => view !== 'accounts')
}

export function normalizeKeyUsageView(value?: string, views: readonly KeyUsageView[] = KEY_USAGE_VIEWS): KeyUsageView {
  return views.includes(value as KeyUsageView) ? value as KeyUsageView : 'overview'
}
