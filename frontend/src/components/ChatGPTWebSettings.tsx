import { useCallback, useEffect, useState } from 'react'
import { Link } from 'react-router-dom'
import { useTranslation } from 'react-i18next'
import { api, type ChatGPTWebDeployment, type ChatGPTWebExecution, type ChatGPTWebState } from '../api'
import type { AccountRow } from '../types'
import { webAccountStatusMessage } from '../lib/chatgptWebStatus'
import { Button } from './ui/button'
import { Card, CardContent } from './ui/card'

export default function ChatGPTWebSettings() {
  const { t } = useTranslation()
  const [deployment, setDeployment] = useState<ChatGPTWebDeployment | null>(null)
  const [accounts, setAccounts] = useState<AccountRow[]>([])
  const [states, setStates] = useState<ChatGPTWebState[]>([])
  const [executions, setExecutions] = useState<ChatGPTWebExecution[]>([])
  const [busy, setBusy] = useState(false)
  const [error, setError] = useState('')
  const load = useCallback(async () => {
    setBusy(true); setError('')
    try {
      const [d, a, s, e] = await Promise.all([api.getChatGPTWebStatus(), api.getAccounts({ channel: 'codex', view: 'lite' }), api.getChatGPTWebAccounts(), api.getChatGPTWebExecutions()])
      setDeployment(d); setAccounts(a.accounts); setStates(s.accounts); setExecutions(e.executions.slice(0, 50))
    } catch { setError('chatgptWeb.loadError') } finally { setBusy(false) }
  }, [])
  useEffect(() => { void load() }, [load])
  const action = async (id: number, enabled: boolean, check: boolean) => {
    setBusy(true); setError('')
    try {
      const state = check ? await api.checkChatGPTWebAccount(id) : await api.setChatGPTWebAccount(id, enabled)
      setStates(old => [...old.filter(item => item.account_id !== id), state])
    } catch { setError('chatgptWeb.actionError') } finally { setBusy(false) }
  }
  return <Card className="gap-0 py-0"><CardContent className="space-y-4 p-5">
    <h3 className="font-semibold">{t('chatgptWeb.title')}</h3>
    <p className="text-xs text-muted-foreground">{t('chatgptWeb.config')}</p>
    <p className="text-sm leading-relaxed">{t('chatgptWeb.guide')}</p>
    <div className="flex flex-wrap gap-3 text-sm"><Link className="text-primary underline" to="/api-keys">{t('chatgptWeb.createKey')}</Link><Link className="text-primary underline" to="/images/studio">{t('chatgptWeb.studio')}</Link></div>
    <p className="text-sm">{t('chatgptWeb.deployment')}: {deployment ? `${t(deployment.enabled ? 'chatgptWeb.enabled' : 'chatgptWeb.disabled')} · ${t(deployment.configured ? 'chatgptWeb.configured' : 'chatgptWeb.notConfigured')} · ${deployment.protocol_version || '—'} · ${(deployment.supported_models || []).join(', ') || '—'}` : '—'}</p>
    {deployment && (!deployment.enabled || !deployment.configured) && <p className="text-sm text-muted-foreground" role="status">{t('chatgptWeb.serviceNotReady')}</p>}
    <Button type="button" variant="outline" size="sm" disabled={busy} onClick={() => void load()}>{t('chatgptWeb.refresh')}</Button>
    {error && <p role="alert" className="text-sm text-destructive">{t(error)}</p>}
    <div className="max-h-96 space-y-2 overflow-auto">
      {accounts.filter(a => !a.openai_responses_api && !a.grok_api && !a.antigravity_api && !a.claude_api).map(account => {
        const state = states.find(s => s.account_id === account.id)
        return <div key={account.id} className="space-y-2 rounded-md border p-3 text-sm">
          <p className="font-medium">{t('chatgptWeb.account')} #{account.id} {account.name || account.email}</p>
          <p>{t('chatgptWeb.status')}: {t(webAccountStatusMessage(state ?? null))} · {t(state?.enabled ? 'chatgptWeb.enabled' : 'chatgptWeb.disabled')}</p>
          <p>{t('chatgptWeb.capabilities')}: {state?.capabilities?.join(', ') || t('chatgptWeb.unknown')} · {t('chatgptWeb.quota')}: {state?.image_quota_remaining ?? t('chatgptWeb.unknown')}</p>
          <p>{t('chatgptWeb.reset')}: {state?.quota_reset_at || '—'} · {t('chatgptWeb.observed')}: {state?.observed_at || '—'} · {t('chatgptWeb.error')}: {state?.error_code || '—'}</p>
          <div className="flex flex-wrap gap-2"><Button type="button" variant="outline" size="sm" disabled={busy} onClick={() => void action(account.id, !state?.enabled, false)}>{t(state?.enabled ? 'chatgptWeb.disable' : 'chatgptWeb.enable')}</Button><Button type="button" variant="outline" size="sm" disabled={busy || !state?.enabled} onClick={() => void action(account.id, true, true)}>{t('chatgptWeb.check')}</Button></div>
        </div>
      })}
    </div>
    <h4 className="font-medium">{t('chatgptWeb.executions')}</h4>
    <div className="max-h-64 space-y-1 overflow-auto text-xs">{executions.length ? executions.map(e => <p key={e.id}>#{e.account_id} · {e.operation} · {e.stage} · {e.error_code || '—'} · {e.updated_at} {e.result_unknown && t('chatgptWeb.resultUnknown')}</p>) : t('chatgptWeb.empty')}</div>
  </CardContent></Card>
}
