import { useEffect, useRef, useState } from 'react'
import { useTranslation } from 'react-i18next'
import { api } from '../api'
import type { ProxySubscription, ProxySubscriptionEngine, ProxySubscriptionInput, ProxySubscriptionsView } from '../types'
import { getErrorMessage } from '../utils/error'
import { Button } from './ui/button'
import { Card, CardContent } from './ui/card'
import { Input } from './ui/input'

const defaultEngine: ProxySubscriptionEngine = { binary_path: '', auto_install: true, controller_port: 19090, port_start: 20000 }
const emptySubscription: ProxySubscriptionInput = { name: '', url: '', enabled: true, include_regex: '', exclude_regex: '', dedupe_regex: '', prefer_regex: '', refresh_minutes: 60 }
const safeError = (value: string) => value.replace(/\b[a-z][a-z0-9+.-]*:\/\/[^\s]+/gi, '[redacted]')

export default function ProxySubscriptions({ onChanged }: { onChanged: () => void }) {
  const { t } = useTranslation()
  const [view, setView] = useState<ProxySubscriptionsView | null>(null)
  const [engine, setEngine] = useState(defaultEngine)
  const [draft, setDraft] = useState<ProxySubscriptionInput | null>(null)
  const [editing, setEditing] = useState<string | null>(null)
  const [urlSet, setUrlSet] = useState(false)
  const [busy, setBusy] = useState(false)
  const [error, setError] = useState('')
  const [notice, setNotice] = useState('')
  const [revision, setRevision] = useState(0)
  const initialized = useRef(false)
  const waiting = useRef(false)
  const onChangedRef = useRef(onChanged)
  onChangedRef.current = onChanged

  useEffect(() => {
    const controller = new AbortController()
    let timer: number | undefined
    const load = async () => {
      try {
        const next = await api.getProxySubscriptions(controller.signal)
        if (controller.signal.aborted) return
        setView(next)
        if (!initialized.current) { setEngine(next.engine); initialized.current = true }
        if (next.operation?.running) { waiting.current = true; timer = window.setTimeout(load, 1500) }
        else if (waiting.current) { waiting.current = false; onChangedRef.current() }
      } catch (err) {
        if (controller.signal.aborted) return
        setError(safeError(getErrorMessage(err)))
        if (waiting.current) timer = window.setTimeout(load, 1500)
      }
    }
    void load()
    return () => { controller.abort(); if (timer !== undefined) window.clearTimeout(timer) }
  }, [revision])

  const disabled = busy || !view || Boolean(view.operation?.running) || waiting.current
  const act = async (action: () => Promise<unknown>, asyncOperation = false, saved = false) => {
    setBusy(true); setError(''); setNotice('')
    try {
      await action()
      if (asyncOperation) waiting.current = true
      if (saved) setNotice(t('proxySubscriptions.savedNotRefreshed'))
      setRevision(value => value + 1)
    } catch (err) { setError(safeError(getErrorMessage(err))); setRevision(value => value + 1) } finally { setBusy(false) }
  }
  const edit = (item: ProxySubscription) => {
    setEditing(item.id); setUrlSet(item.url_set)
    setDraft({ name: item.name, url: '', enabled: item.enabled, include_regex: item.include_regex || '', exclude_regex: item.exclude_regex || '', dedupe_regex: item.dedupe_regex || '', prefer_regex: item.prefer_regex || '', refresh_minutes: item.refresh_minutes })
  }
  const validMinutes = draft && Number.isInteger(draft.refresh_minutes) && (draft.refresh_minutes === 0 || draft.refresh_minutes >= 5)
  const validEngine = [engine.controller_port, engine.port_start].every(port => Number.isInteger(port) && port > 0 && port <= 65535)
  return <Card><CardContent className="space-y-3 p-4 sm:p-5">
    <div className="flex flex-wrap items-center justify-between gap-3">
      <div><h2 className="font-semibold">{t('proxySubscriptions.title')}</h2>{view && <p className="text-sm text-muted-foreground">{t('proxySubscriptions.summary', { count: view.subscriptions?.length ?? 0 })} · {view.running ? t('proxySubscriptions.running') : t('proxySubscriptions.stopped')}{view.version ? ` · ${view.version}` : ''}</p>}</div>
      <div className="flex flex-wrap gap-2"><Button variant="outline" size="sm" disabled={disabled} onClick={() => void act(() => api.startProxySubscriptionEngine(), true)}>{t('proxySubscriptions.start')}</Button><Button variant="outline" size="sm" disabled={disabled} onClick={() => void act(() => api.refreshProxySubscriptions(), true)}>{t('proxySubscriptions.refreshAll')}</Button></div>
    </div>
    {(view?.operation?.running || waiting.current) && <p role="status" className="text-sm text-primary">{t('proxySubscriptions.progress')}: {view?.operation?.stage || t('proxySubscriptions.starting')}</p>}
    {view?.operation?.finished_at && !view.operation.running && <p className="text-xs text-muted-foreground">{t('proxySubscriptions.lastOperation')}: {view.operation.stage} · {view.operation.finished_at}</p>}
    {[error, view?.last_error, view?.operation?.error].filter(Boolean).map((message, index) => <p key={index} role="alert" className="break-words text-sm text-destructive">{safeError(message!)}</p>)}
    {error && !view && <Button variant="outline" size="sm" onClick={() => { setError(''); setRevision(value => value + 1) }}>{t('proxySubscriptions.refresh')}</Button>}
    {notice && <p role="status" className="text-sm text-muted-foreground">{notice}</p>}
    <details className="group border-t pt-3"><summary className="cursor-pointer text-sm font-medium">{t('proxySubscriptions.settings')}</summary>
      <div className="space-y-4 pt-4">
        <p className="text-sm text-muted-foreground">{t('proxySubscriptions.engineHint')}</p>
        <div className="grid gap-3 sm:grid-cols-3">
          <label className="space-y-1 text-sm sm:col-span-3"><span>{t('proxySubscriptions.binaryPath')}</span><Input value={engine.binary_path} disabled={disabled} onChange={e => setEngine({ ...engine, binary_path: e.target.value })} /></label>
          <label className="space-y-1 text-sm"><span>{t('proxySubscriptions.controllerPort')}</span><Input type="number" min={1} max={65535} value={engine.controller_port} disabled={disabled} onChange={e => setEngine({ ...engine, controller_port: Number(e.target.value) })} /></label>
          <label className="space-y-1 text-sm"><span>{t('proxySubscriptions.portStart')}</span><Input type="number" min={1} max={65535} value={engine.port_start} disabled={disabled} onChange={e => setEngine({ ...engine, port_start: Number(e.target.value) })} /></label>
          <label className="flex items-center gap-2 text-sm"><input type="checkbox" checked={engine.auto_install} disabled={disabled} onChange={e => setEngine({ ...engine, auto_install: e.target.checked })} />{t('proxySubscriptions.autoInstall')}</label>
        </div>
        <Button variant="outline" size="sm" disabled={disabled || !validEngine} onClick={() => void act(() => api.saveProxySubscriptionEngine(engine), false, true)}>{t('proxySubscriptions.saveEngine')}</Button>
        <div className="space-y-2 border-t pt-4">
          <div className="flex items-center justify-between gap-2"><h3 className="text-sm font-semibold">{t('proxySubscriptions.title')}</h3><Button variant="outline" size="sm" disabled={disabled} onClick={() => { setEditing(null); setUrlSet(false); setDraft({ ...emptySubscription }) }}>{t('proxySubscriptions.add')}</Button></div>
          {(view?.subscriptions ?? []).map(item => <div key={item.id} className="space-y-2 rounded-md border p-3">
            <div className="flex flex-wrap items-center justify-between gap-2"><div className="min-w-0"><span className="font-medium">{item.name}</span> <span className="text-xs text-muted-foreground">{item.enabled ? t('proxySubscriptions.enabled') : t('proxySubscriptions.disabled')} · {t('proxySubscriptions.nodes', { kept: item.kept_nodes, total: item.total_nodes })}</span></div><div className="flex flex-wrap gap-1"><Button variant="outline" size="sm" disabled={disabled} onClick={() => edit(item)}>{t('proxySubscriptions.edit')}</Button><Button variant="outline" size="sm" disabled={disabled} onClick={() => void act(() => api.refreshProxySubscriptions(item.id), true)}>{t('proxySubscriptions.refresh')}</Button><Button variant="outline" size="sm" disabled={disabled} onClick={() => { if (window.confirm(t('proxySubscriptions.confirmDelete', { name: item.name }))) void act(() => api.deleteProxySubscription(item.id), true) }}>{t('proxySubscriptions.delete')}</Button></div></div>
            {item.last_updated && <p className="text-xs text-muted-foreground">{t('proxySubscriptions.lastUpdated')}: {item.last_updated}</p>}{item.last_error && <p role="alert" className="break-words text-sm text-destructive">{safeError(item.last_error)}</p>}
          </div>)}
          {draft && <form className="space-y-3 rounded-md border bg-muted/20 p-3" onSubmit={e => { e.preventDefault(); if (!validMinutes || disabled) return; void act(async () => { if (editing == null) await api.createProxySubscription(draft); else await api.updateProxySubscription(editing, draft); setDraft(null) }, false, true) }}>
            <h4 className="font-medium">{t(editing == null ? 'proxySubscriptions.add' : 'proxySubscriptions.edit')}</h4>
            <div className="grid gap-3 sm:grid-cols-2">
              <label className="space-y-1 text-sm"><span>{t('proxySubscriptions.name')}</span><Input required value={draft.name} disabled={disabled} onChange={e => setDraft({ ...draft, name: e.target.value })} /></label>
              <label className="space-y-1 text-sm"><span>{t('proxySubscriptions.url')}</span><Input type="password" autoComplete="new-password" required={editing == null || !urlSet} placeholder={urlSet ? t('proxySubscriptions.urlPreserved') : ''} value={draft.url} disabled={disabled} onChange={e => setDraft({ ...draft, url: e.target.value })} /></label>
              {(['include_regex', 'exclude_regex', 'dedupe_regex', 'prefer_regex'] as const).map(field => <label key={field} className="space-y-1 text-sm"><span>{t(`proxySubscriptions.${field}`)}</span><Input value={draft[field]} disabled={disabled} onChange={e => setDraft({ ...draft, [field]: e.target.value })} /></label>)}
              <label className="space-y-1 text-sm"><span>{t('proxySubscriptions.refreshMinutes')}</span><Input type="number" min={0} step={1} value={draft.refresh_minutes} disabled={disabled} onChange={e => setDraft({ ...draft, refresh_minutes: e.target.value === '' ? -1 : Number(e.target.value) })} /></label>
              <label className="flex items-center gap-2 text-sm"><input type="checkbox" checked={draft.enabled} disabled={disabled} onChange={e => setDraft({ ...draft, enabled: e.target.checked })} />{t('proxySubscriptions.enabled')}</label>
            </div>
            <p className="text-xs text-muted-foreground">{t('proxySubscriptions.filterHint')}</p><p className="text-xs text-muted-foreground">{t('proxySubscriptions.dedupeHint')}</p>
            {!validMinutes && <p role="alert" className="text-sm text-destructive">{t('proxySubscriptions.minutesInvalid')}</p>}
            <div className="flex gap-2"><Button type="submit" size="sm" disabled={disabled || !validMinutes}>{t('proxySubscriptions.save')}</Button><Button type="button" variant="outline" size="sm" disabled={busy} onClick={() => setDraft(null)}>{t('proxySubscriptions.cancel')}</Button></div>
          </form>}
        </div>
      </div>
    </details>
  </CardContent></Card>
}
