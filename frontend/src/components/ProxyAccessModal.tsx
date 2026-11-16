import { useEffect, useState } from 'react'
import { useTranslation } from 'react-i18next'
import { api, type ProxyRow } from '../api'
import type { AccountRow } from '../types'
import { getErrorMessage } from '../utils/error'
import Modal from './Modal'
import { Button } from './ui/button'
import { Input } from './ui/input'

export default function ProxyAccessModal({ proxy, onClose, onSaved }: { proxy: ProxyRow | null; onClose: () => void; onSaved: () => void }) {
  const { t } = useTranslation()
  const [exclusive, setExclusive] = useState(false)
  const [selected, setSelected] = useState<Set<number>>(new Set())
  const [accounts, setAccounts] = useState<AccountRow[]>([])
  const [search, setSearch] = useState('')
  const [page, setPage] = useState(1)
  const [total, setTotal] = useState(0)
  const [error, setError] = useState('')
  const [loading, setLoading] = useState(false)
  const [saving, setSaving] = useState(false)

  useEffect(() => {
    if (!proxy) return
    setExclusive(Boolean(proxy.exclusive))
    setSelected(new Set(proxy.allowed_account_ids ?? []))
    setSearch(''); setPage(1); setError(''); setAccounts([])
  }, [proxy])

  useEffect(() => {
    if (!proxy) return
    const controller = new AbortController()
    setLoading(true)
    api.getAccountsPage({ page, pageSize: 20, search }, controller.signal).then(result => {
      if (!controller.signal.aborted) { setAccounts(result.accounts ?? []); setTotal(result.total ?? 0) }
    }).catch(err => { if (!controller.signal.aborted) setError(getErrorMessage(err)) })
      .finally(() => { if (!controller.signal.aborted) setLoading(false) })
    return () => controller.abort()
  }, [proxy, page, search])

  async function save() {
    if (!proxy || (exclusive && selected.size === 0)) return
    setSaving(true); setError('')
    try {
      await api.updateProxyAccess(proxy.id, { exclusive, allowed_account_ids: Array.from(selected) })
      onSaved(); onClose()
    } catch (err) { setError(getErrorMessage(err)) } finally { setSaving(false) }
  }

  return <Modal show={!!proxy} title={t('proxySubscriptions.accessTitle')} onClose={onClose} footer={<>
    <Button variant="outline" onClick={onClose} disabled={saving}>{t('proxySubscriptions.cancel')}</Button>
    <Button onClick={() => void save()} disabled={saving || (exclusive && selected.size === 0)}>{t('proxySubscriptions.save')}</Button>
  </>}>
    <div className="space-y-4">
      <p className="text-sm text-muted-foreground">{t('proxySubscriptions.accessHint')}</p>
      <label className="flex items-center gap-2 text-sm"><input type="checkbox" checked={exclusive} onChange={e => setExclusive(e.target.checked)} />{t('proxySubscriptions.exclusive')}</label>
      {exclusive && <>
        <p className="text-sm">{t('proxySubscriptions.selectedCount', { count: selected.size })}</p>
        <Input value={search} onChange={e => { setSearch(e.target.value); setPage(1) }} placeholder={t('proxySubscriptions.searchAccounts')} aria-label={t('proxySubscriptions.searchAccounts')} />
        <div className="max-h-64 space-y-1 overflow-y-auto rounded-md border p-2" aria-busy={loading}>
          {accounts.map(account => <label key={account.id} className="flex items-center gap-2 rounded p-1.5 text-sm hover:bg-muted">
            <input type="checkbox" checked={selected.has(account.id)} onChange={e => { const checked = e.target.checked; setSelected(current => { const next = new Set(current); if (checked) next.add(account.id); else next.delete(account.id); return next }) }} />
            <span className="break-all">{account.email || account.name || `#${account.id}`} <span className="text-muted-foreground">#{account.id}</span></span>
          </label>)}
          {!loading && accounts.length === 0 && <p className="p-2 text-sm text-muted-foreground">{t('proxySubscriptions.noAccounts')}</p>}
        </div>
        <div className="flex items-center justify-between gap-2 text-sm"><Button variant="outline" size="sm" disabled={page <= 1 || loading} onClick={() => setPage(page - 1)}>{t('proxySubscriptions.previous')}</Button><span>{t('proxySubscriptions.page', { page, total })}</span><Button variant="outline" size="sm" disabled={page * 20 >= total || loading} onClick={() => setPage(page + 1)}>{t('proxySubscriptions.next')}</Button></div>
        {selected.size === 0 && <p className="text-sm text-destructive">{t('proxySubscriptions.requireAccount')}</p>}
      </>}
      {error && <p role="alert" className="break-words text-sm text-destructive">{error}</p>}
    </div>
  </Modal>
}
