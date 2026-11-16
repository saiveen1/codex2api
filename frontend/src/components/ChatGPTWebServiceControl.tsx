import { useEffect, useId, useState } from 'react'
import { useTranslation } from 'react-i18next'
import { api, type ChatGPTWebConfig, type ChatGPTWebDeployment } from '../api'
import { Button } from './ui/button'
import { Input } from './ui/input'

export default function ChatGPTWebServiceControl({ deployment, onChange }: {
  deployment: ChatGPTWebDeployment
  onChange: (next: ChatGPTWebDeployment) => void
}) {
  const { t } = useTranslation()
  const id = useId()
  const control = deployment.control
  const externalSecretSet = control?.config.mode === 'external' && control.secret_set
  const [draft, setDraft] = useState<ChatGPTWebConfig | null>(null)
  const [busy, setBusy] = useState(false)
  const [error, setError] = useState('')
  const [saved, setSaved] = useState(false)
  useEffect(() => {
    if (control) setDraft({ ...control.config, worker_secret: '' })
  }, [control?.config.enabled, control?.config.mode, control?.config.worker_url, control?.config.timeout_seconds])

  if (!control || !draft) return <p role="status" className="text-sm text-muted-foreground">{t('chatgptWeb.controlUnavailable')}</p>
  const transitioning = control.stage === 'starting' || control.stage === 'draining'
  const change = (next: Partial<ChatGPTWebConfig>) => {
    setDraft({ ...draft, ...next }); setSaved(false); setError('')
  }
  const save = async (enabled: boolean, useSaved = false) => {
    setBusy(true); setError(''); setSaved(false)
    try {
      const next = await api.configureChatGPTWeb({ ...(useSaved ? control.config : draft), enabled, worker_secret: useSaved ? '' : draft.worker_secret })
      onChange(next)
      if (next.control) setDraft({ ...next.control.config, worker_secret: '' })
      setSaved(true)
    } catch (cause) {
      setError(cause instanceof Error ? cause.message : t('chatgptWeb.controlError'))
      // A persisted desired configuration can outlive a failed startup. Re-read
      // actual state; do not retry a mutating request after a lost response.
      try { onChange(await api.getChatGPTWebStatus()) } catch { /* Keep the original error. */ }
    } finally { setBusy(false) }
  }

  return <form className="space-y-3 rounded-md border p-4" onSubmit={event => { event.preventDefault(); void save(true) }} aria-busy={busy}>
    <div className="flex flex-wrap items-center justify-between gap-2">
      <h4 className="text-sm font-medium">{t('chatgptWeb.serviceControl')}</h4>
      <span role="status" className="text-sm">{t(`chatgptWeb.runtimeStages.${control.stage}`, { defaultValue: control.stage })}</span>
    </div>
    <div className="grid gap-3 sm:grid-cols-2">
      <label className="space-y-1 text-sm" htmlFor={`${id}-mode`}>
        <span>{t('chatgptWeb.mode')}</span>
        <select id={`${id}-mode`} aria-label={t('chatgptWeb.mode')} className="h-9 w-full rounded-md border bg-background px-3" value={draft.mode} disabled={busy || transitioning || control.config.enabled || control.running} onChange={event => change({ mode: event.target.value as ChatGPTWebConfig['mode'] })}>
          <option value="managed">{t('chatgptWeb.managed')}</option>
          <option value="external">{t('chatgptWeb.external')}</option>
        </select>
      </label>
      <label className="space-y-1 text-sm" htmlFor={`${id}-timeout`}>
        <span>{t('chatgptWeb.timeout')}</span>
        <Input id={`${id}-timeout`} type="number" min={10} max={600} step={1} required value={draft.timeout_seconds} disabled={busy || transitioning} onChange={event => change({ timeout_seconds: Number(event.target.value) })} />
      </label>
    </div>
    {draft.mode === 'managed' ? <p className="text-xs text-muted-foreground">{t(control.available ? 'chatgptWeb.managedHint' : 'chatgptWeb.managedMissing')}</p> : <div className="grid gap-3 sm:grid-cols-2">
      <label className="space-y-1 text-sm" htmlFor={`${id}-url`}>
        <span>{t('chatgptWeb.workerURL')}</span>
        <Input id={`${id}-url`} type="url" required value={draft.worker_url} disabled={busy || transitioning} placeholder="http://127.0.0.1:8091" onChange={event => change({ worker_url: event.target.value })} />
      </label>
      <label className="space-y-1 text-sm" htmlFor={`${id}-secret`}>
        <span>{t('chatgptWeb.workerSecret')}</span>
        <Input id={`${id}-secret`} type="password" autoComplete="new-password" minLength={16} required={!externalSecretSet} value={draft.worker_secret || ''} disabled={busy || transitioning} placeholder={t(externalSecretSet ? 'chatgptWeb.secretKeep' : 'chatgptWeb.secretRequired')} onChange={event => change({ worker_secret: event.target.value })} />
      </label>
      <p className="text-xs text-muted-foreground sm:col-span-2">{t('chatgptWeb.externalHint')}</p>
    </div>}
    <div className="flex flex-wrap gap-2">
      <Button type="submit" size="sm" disabled={busy || transitioning || (draft.mode === 'managed' && !control.available)}>{t('chatgptWeb.saveEnable')}</Button>
      <Button type="button" variant="outline" size="sm" disabled={busy || !control.config.enabled} onClick={() => void save(false, true)}>{t('chatgptWeb.stopService')}</Button>
    </div>
    <p className="text-xs text-muted-foreground">{t('chatgptWeb.stopHint')}</p>
    {saved && <p role="status" className="text-sm">{t('chatgptWeb.controlSaved')}</p>}
    {(error || control.last_error) && <p role="alert" className="break-words text-sm text-destructive">{error || control.last_error}</p>}
  </form>
}
