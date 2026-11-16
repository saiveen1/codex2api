import { useEffect, useState } from 'react'
import { Link } from 'react-router-dom'
import { useTranslation } from 'react-i18next'
import { Eye, RefreshCw } from 'lucide-react'
import { api, type ChatGPTWebDeployment, type ChatGPTWebState } from '../api'
import { webAccountStatus, webAccountStatusMessage, webImageQuotaRemaining } from '../lib/chatgptWebStatus'
import { formatBeijingTime } from '../utils/time'
import { Button } from './ui/button'
import { Dialog, DialogContent, DialogDescription, DialogFooter, DialogHeader, DialogTitle, DialogTrigger } from './ui/dialog'

export default function ChatGPTWebAccountBadge({ accountID, accountName, state, onStateChange }: {
  accountID: number
  accountName?: string
  state?: ChatGPTWebState | null
  onStateChange?: (state: ChatGPTWebState | null) => void
}) {
  const { t } = useTranslation()
  const [open, setOpen] = useState(false)
  const [snapshot, setSnapshot] = useState(state)
  const [deployment, setDeployment] = useState<ChatGPTWebDeployment | null>(null)
  const [loading, setLoading] = useState(false)
  const [refreshing, setRefreshing] = useState(false)
  const [error, setError] = useState('')
  useEffect(() => { setSnapshot(state) }, [state])
  const status = webAccountStatus(snapshot)
  const remaining = webImageQuotaRemaining(snapshot)
  const busy = loading || refreshing
  const serviceReady = deployment?.enabled === true && deployment.configured === true
  const canRefresh = serviceReady && snapshot?.enabled === true
  const label = accountName || `#${accountID}`

  const loadSnapshot = async () => {
    if (busy) return
    setLoading(true)
    setDeployment(null)
    setError('')
    try {
      const [service, result] = await Promise.all([api.getChatGPTWebStatus(), api.getChatGPTWebAccounts()])
      if (!Array.isArray(result.accounts)) throw new Error('Invalid account state response')
      setDeployment(service)
      const next = result.accounts.find(item => item.account_id === accountID) ?? null
      setSnapshot(next)
      onStateChange?.(next)
    } catch { setError(t('chatgptWeb.quotaReadError')) }
    finally { setLoading(false) }
  }

  const refreshQuota = async () => {
    if (!canRefresh || busy) return
    setRefreshing(true)
    setError('')
    try {
      const result = await api.checkChatGPTWebAccount(accountID)
      if (result.account_id !== accountID) throw new Error('Account state mismatch')
      setSnapshot(result)
      onStateChange?.(result)
    } catch { setError(t('chatgptWeb.quotaRefreshError')) }
    finally { setRefreshing(false) }
  }

  return <Dialog open={open} onOpenChange={next => { setOpen(next); if (next) void loadSnapshot() }}>
    <DialogTrigger asChild>
      <button type="button" aria-label={t('chatgptWeb.viewQuotaFor', { account: label })} title={t('chatgptWeb.viewQuotaFor', { account: label })} className="inline-flex max-w-full flex-wrap items-center gap-1 rounded border border-border px-1.5 py-0.5 text-[11px] font-medium text-muted-foreground hover:text-foreground focus-visible:outline-none focus-visible:ring-2 focus-visible:ring-ring" onClick={event => event.stopPropagation()}>
        <span>{t('chatgptWeb.accountBadge', { status: t(`chatgptWeb.states.${status}`) })}</span>
        <span>· {t('chatgptWeb.quota')}: {remaining === null ? t('chatgptWeb.unknown') : remaining}</span>
        <Eye className="size-3 shrink-0" aria-hidden="true" />
      </button>
    </DialogTrigger>
    <DialogContent className="sm:max-w-lg" onClick={event => event.stopPropagation()}>
      <DialogHeader>
        <DialogTitle>{t('chatgptWeb.quotaTitle')}</DialogTitle>
        <DialogDescription>{label} · {t('chatgptWeb.quotaDescription')}</DialogDescription>
      </DialogHeader>
      <dl className="grid grid-cols-2 gap-4 text-sm" aria-busy={busy}>
        <div className="col-span-2 rounded-md border border-border bg-muted/20 p-4">
          <dt className="text-muted-foreground">{t('chatgptWeb.quotaRemaining')}</dt>
          <dd className="mt-1 text-3xl font-semibold tabular-nums" data-testid="web-image-quota-remaining" aria-live="polite">{remaining === null ? t('chatgptWeb.unknown') : remaining}</dd>
        </div>
        <div><dt className="text-muted-foreground">{t('chatgptWeb.reset')}</dt><dd className="mt-1 break-words">{snapshot?.quota_reset_at ? formatBeijingTime(snapshot.quota_reset_at) : '—'}</dd></div>
        <div><dt className="text-muted-foreground">{t('chatgptWeb.observed')}</dt><dd className="mt-1 break-words">{snapshot?.observed_at ? formatBeijingTime(snapshot.observed_at) : '—'}</dd></div>
      </dl>
      <p className="text-sm text-muted-foreground">{t(webAccountStatusMessage(snapshot))}</p>
      {!loading && deployment && !serviceReady && <p className="text-sm text-muted-foreground">{t('chatgptWeb.serviceNotReady')}</p>}
      <p className="text-xs leading-relaxed text-muted-foreground">{t('chatgptWeb.quotaSnapshotHint')}</p>
      {error && <p role="alert" className="text-sm text-destructive">{error}</p>}
      <DialogFooter className="flex-wrap gap-2 sm:justify-between">
        <Link to="/settings#settings-codex-images" className="self-center text-sm text-primary underline">{t('chatgptWeb.openSettings')}</Link>
        <div className="flex flex-wrap gap-2">
          <Button type="button" variant="outline" disabled={busy} onClick={() => void loadSnapshot()}>{t('chatgptWeb.reloadQuotaState')}</Button>
          <Button type="button" disabled={busy || !canRefresh} onClick={() => void refreshQuota()}>
            <RefreshCw className={refreshing ? 'size-4 animate-spin' : 'size-4'} />{t('chatgptWeb.refreshQuota')}
          </Button>
        </div>
      </DialogFooter>
    </DialogContent>
  </Dialog>
}
