import { useEffect, useState } from 'react'
import { useTranslation } from 'react-i18next'
import { api, type ProxyRow } from '../api'
import type { AccountRow } from '../types'
import { getErrorMessage } from '../utils/error'
import Modal from './Modal'
import { Button } from './ui/button'
import { Input } from './ui/input'

type Props = { proxy: ProxyRow | null; onClose: () => void; onSaved: () => void }

export default function ProxyAccessModal({ proxy, ...callbacks }: Props) {
  return proxy ? <ProxyAccessEditor key={proxy.id} proxy={proxy} {...callbacks} /> : null
}

function ProxyAccessEditor({ proxy, onClose, onSaved }: Omit<Props, 'proxy'> & { proxy: ProxyRow }) {
  const { t } = useTranslation()
  const [exclusive, setExclusive] = useState(Boolean(proxy.exclusive))
  const [selected, setSelected] = useState<Set<number>>(() => new Set(proxy.allowed_account_ids ?? []))
  const [accounts, setAccounts] = useState<AccountRow[]>([])
  const [boundAccounts, setBoundAccounts] = useState<AccountRow[]>([])
  const [search, setSearch] = useState('')
  const [page, setPage] = useState(1)
  const [total, setTotal] = useState(0)
  const [error, setError] = useState('')
  const [loading, setLoading] = useState(false)
  const [saving, setSaving] = useState(false)
  const [loadingBound, setLoadingBound] = useState(true)
  const [boundError, setBoundError] = useState(false)
  const [boundAttempt, setBoundAttempt] = useState(0)

  useEffect(() => {
    const controller = new AbortController()
    setLoading(true)
    api.getAccountsPage({ page, pageSize: 20, search }, controller.signal).then(result => {
      if (!controller.signal.aborted) { setAccounts(result.accounts ?? []); setTotal(result.total ?? 0) }
    }).catch(err => { if (!controller.signal.aborted) setError(getErrorMessage(err)) })
      .finally(() => { if (!controller.signal.aborted) setLoading(false) })
    return () => controller.abort()
  }, [page, search])

  useEffect(() => {
    const controller = new AbortController()
    async function loadBoundAccounts() {
      setLoadingBound(true); setBoundError(false)
      if (!proxy.url) throw new Error('Missing proxy URL')
      const bound: AccountRow[] = []
      const ids = new Set<number>()
      let expectedTotal: number | undefined
      for (let boundPage = 1; ; boundPage++) {
        const result = await api.getAccountsPage({ page: boundPage, pageSize: 100, proxyUrl: proxy.url, proxyFilter: 'this', sort: 'id', order: 'asc' }, controller.signal)
        if (controller.signal.aborted) return
        if (!Number.isSafeInteger(result.total) || result.total < 0 || !Array.isArray(result.accounts)) throw new Error('Invalid binding page')
        expectedTotal ??= result.total
        if (result.total !== expectedTotal) throw new Error('Bindings changed during loading')
        for (const account of result.accounts) {
          if (!Number.isSafeInteger(account.id) || account.id <= 0 || ids.has(account.id)) throw new Error('Invalid or repeated bound account')
          ids.add(account.id); bound.push(account)
        }
        if (bound.length === expectedTotal) break
        if (bound.length > expectedTotal || result.accounts.length === 0) throw new Error('Incomplete binding pages')
      }
      // Apply only a complete snapshot; a partial read must not drop a binding.
      setBoundAccounts(bound)
      setSelected(current => new Set([...current, ...ids]))
      if (bound.length > 0) setExclusive(true)
    }
    void loadBoundAccounts().catch(() => {
      if (!controller.signal.aborted) setBoundError(true)
    }).finally(() => {
      if (!controller.signal.aborted) setLoadingBound(false)
    })
    return () => controller.abort()
  }, [proxy.url, boundAttempt])

  const selectionBlocked = saving || loadingBound || boundError
  async function save() {
    if (selectionBlocked) return
    setSaving(true); setError('')
    try {
      await api.updateProxyAccess(proxy.id, { exclusive, allowed_account_ids: exclusive ? Array.from(selected) : [] })
      onSaved(); onClose()
    } catch (err) { setError(getErrorMessage(err)) } finally { setSaving(false) }
  }

  function accountChoice(account: AccountRow) {
    return <label key={account.id} className="flex items-center gap-2 rounded p-1.5 text-sm hover:bg-muted">
      <input type="checkbox" disabled={selectionBlocked || boundIDs.has(account.id)} checked={selected.has(account.id)} onChange={e => {
        const checked = e.target.checked
        if (checked) setExclusive(true)
        setSelected(current => { const next = new Set(current); if (checked) next.add(account.id); else next.delete(account.id); return next })
      }} />
      <span className="break-all">{account.email || account.name || `#${account.id}`} <span className="text-muted-foreground">#{account.id}</span></span>
    </label>
  }
  const boundIDs = new Set(boundAccounts.map(account => account.id))

  return <Modal show title={t('proxySubscriptions.accessTitle')} onClose={onClose} footer={<>
    <Button variant="outline" onClick={onClose} disabled={saving}>{t('proxySubscriptions.cancel')}</Button>
    <Button onClick={() => void save()} disabled={selectionBlocked}>{t('proxySubscriptions.save')}</Button>
  </>}>
    <div className="space-y-4">
      <p className="text-sm text-muted-foreground">{t('proxySubscriptions.accessHint')}</p>
      <label className="flex items-center gap-2 text-sm"><input type="checkbox" checked={exclusive} disabled={selectionBlocked || boundIDs.size > 0} onChange={e => {
        setExclusive(e.target.checked)
        if (!e.target.checked) setSelected(new Set())
      }} />{t('proxySubscriptions.exclusive')}</label>
      <div className="space-y-3">
        {loadingBound && <p role="status" className="text-sm text-muted-foreground">{t('proxySubscriptions.loadingBound')}</p>}
        {boundError && <div role="alert" className="space-y-2 text-sm text-destructive"><p>{t('proxySubscriptions.boundLoadFailed')}</p><Button variant="outline" size="sm" onClick={() => setBoundAttempt(n => n + 1)} disabled={loadingBound}>{t('proxySubscriptions.retryBound')}</Button></div>}
        <p className="text-sm">{t('proxySubscriptions.selectedCount', { count: selected.size })}</p>
        {boundAccounts.length > 0 && <div className="space-y-1">
          <p className="text-xs text-muted-foreground">{t('proxySubscriptions.boundAccounts')}</p>
          <div className="max-h-32 overflow-y-auto rounded-md border p-2">{boundAccounts.map(accountChoice)}</div>
        </div>}
        <Input value={search} onChange={e => { setSearch(e.target.value); setPage(1) }} placeholder={t('proxySubscriptions.searchAccounts')} aria-label={t('proxySubscriptions.searchAccounts')} disabled={saving} />
        <div className="max-h-64 space-y-1 overflow-y-auto rounded-md border p-2" aria-busy={loading}>
          {accounts.filter(account => !boundIDs.has(account.id)).map(accountChoice)}
          {!loading && accounts.length === 0 && <p className="p-2 text-sm text-muted-foreground">{t('proxySubscriptions.noAccounts')}</p>}
        </div>
        <div className="flex items-center justify-between gap-2 text-sm"><Button variant="outline" size="sm" disabled={page <= 1 || loading || saving} onClick={() => setPage(page - 1)}>{t('proxySubscriptions.previous')}</Button><span>{t('proxySubscriptions.page', { page, total })}</span><Button variant="outline" size="sm" disabled={page * 20 >= total || loading || saving} onClick={() => setPage(page + 1)}>{t('proxySubscriptions.next')}</Button></div>
        {exclusive && !selectionBlocked && selected.size === 0 && <p className="text-sm text-muted-foreground">{t('proxySubscriptions.reservedHint')}</p>}
      </div>
      {error && <p role="alert" className="break-words text-sm text-destructive">{error}</p>}
    </div>
  </Modal>
}
