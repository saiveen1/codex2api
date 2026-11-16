import { useCallback, useEffect, useState } from 'react'
import ChatGPTWebAccountBadge from './ChatGPTWebAccountBadge'
import ChatGPTWebServiceControl from './ChatGPTWebServiceControl'
import { Link } from 'react-router-dom'
import { useTranslation } from 'react-i18next'
import { api, type ChatGPTWebBatchRequest, type ChatGPTWebBatchResult, type ChatGPTWebDeployment, type ChatGPTWebExecution, type ChatGPTWebState } from '../api'
import type { AccountGroup, AccountRow } from '../types'
import { webAccountStatusMessage } from '../lib/chatgptWebStatus'
import { webActionError, webFailureSnapshot } from '../lib/chatgptWebActions'
import { Button } from './ui/button'
import { Card, CardContent } from './ui/card'

export default function ChatGPTWebSettings() {
  const { t } = useTranslation()
  const [deployment, setDeployment] = useState<ChatGPTWebDeployment | null>(null)
  const [accounts, setAccounts] = useState<AccountRow[]>([])
  const [groups, setGroups] = useState<AccountGroup[]>([])
  const [keys, setKeys] = useState<{ id: number; name: string }[]>([])
  const [states, setStates] = useState<ChatGPTWebState[]>([])
  const [executions, setExecutions] = useState<ChatGPTWebExecution[]>([])
  const [scope, setScope] = useState<ChatGPTWebBatchRequest['scope']>('accounts')
  const [selected, setSelected] = useState<number[]>([])
  const [groupID, setGroupID] = useState('')
  const [keyID, setKeyID] = useState('')
  const [summary, setSummary] = useState<ChatGPTWebBatchResult | null>(null)
  const [busy, setBusy] = useState(false)
  const [error, setError] = useState('')
  const load = useCallback(async () => {
    setBusy(true); setError('')
    try {
      const [d, a, s, e, g, k] = await Promise.all([api.getChatGPTWebStatus(), api.getAccounts({ channel: 'codex', view: 'lite' }), api.getChatGPTWebAccounts(), api.getChatGPTWebExecutions(), api.listAccountGroups(), api.getAPIKeys({ view: 'lite' })])
      setDeployment(d); setAccounts(a.accounts); setStates(s.accounts); setExecutions(e.executions.slice(0, 50))
      setGroups(g.groups.filter(group => group.channel === 'codex'))
      setKeys(k.keys.filter(key => key.limits?.upstream_channel === 'chatgpt_web').map(key => ({ id: key.id, name: key.name })))
    } catch (cause) { setError(webActionError(cause, t('chatgptWeb.loadError'))) } finally { setBusy(false) }
  }, [t])
  useEffect(() => { void load() }, [load])
  const updateState = (id: number, next: ChatGPTWebState | null) => setStates(old => next ? [...old.filter(item => item.account_id !== id), next] : old.filter(item => item.account_id !== id))
  const action = async (id: number, enabled: boolean, check: boolean) => {
    if (busy) return
    setBusy(true); setError('')
    try {
      updateState(id, check ? await api.checkChatGPTWebAccount(id) : await api.setChatGPTWebAccount(id, enabled))
    } catch (cause) {
      setError(webActionError(cause, t('chatgptWeb.actionError')))
      const next = await webFailureSnapshot(cause, id)
      if (next !== undefined) updateState(id, next)
    } finally { setBusy(false) }
  }
  const eligibleAccounts = accounts.filter(account => account.account_type === 'oauth')
  const targetIDs = selected.filter(id => eligibleAccounts.some(account => account.id === id))
  const validScope = scope === 'accounts' ? targetIDs.length > 0 && targetIDs.length <= 1000 : scope === 'group' ? groups.some(group => group.id === Number(groupID)) : keys.some(key => key.id === Number(keyID))
  const batchAction = async (enabled: boolean) => {
    if (busy || !validScope) return
    const body: ChatGPTWebBatchRequest = scope === 'accounts' ? { scope, enabled, account_ids: targetIDs } : scope === 'group' ? { scope, enabled, group_id: Number(groupID) } : { scope, enabled, api_key_id: Number(keyID) }
    setBusy(true); setError(''); setSummary(null)
    try {
      const result = await api.batchSetChatGPTWebAccounts(body)
      setSummary(result)
      setStates(previous => {
        const next = new Map(previous.map(state => [state.account_id, state]))
        for (const item of result.results) if (item.state) next.set(item.account_id, item.state)
        return [...next.values()]
      })
    } catch (cause) {
      setError(webActionError(cause, t('chatgptWeb.actionError')))
    } finally {
      // Resolve all current members on the server, including states not returned after a partial failure.
      try { setStates((await api.getChatGPTWebAccounts()).accounts) } catch { /* Keep action errors and returned states. */ }
      setBusy(false)
    }
  }
  return <Card className="min-w-0 gap-0 py-0"><CardContent className="min-w-0 space-y-4 p-5">
    <h3 className="font-semibold">{t('chatgptWeb.title')}</h3>
    <p className="text-xs text-muted-foreground">{t('chatgptWeb.config')}</p>
    {deployment && <ChatGPTWebServiceControl deployment={deployment} onChange={setDeployment} />}
    <p className="text-sm leading-relaxed">{t('chatgptWeb.guide')}</p>
    <div className="flex flex-wrap gap-3 text-sm"><Link className="text-primary underline" to="/api-keys">{t('chatgptWeb.createKey')}</Link><Link className="text-primary underline" to="/images/studio">{t('chatgptWeb.studio')}</Link></div>
    <p className="break-words text-sm">{t('chatgptWeb.deployment')}: {deployment ? `${t(deployment.enabled ? 'chatgptWeb.enabled' : 'chatgptWeb.disabled')} · ${t(deployment.configured ? 'chatgptWeb.configured' : 'chatgptWeb.notConfigured')} · ${deployment.protocol_version || '—'} · ${(deployment.supported_models || []).join(', ') || '—'}` : '—'}</p>
    {deployment && (!deployment.enabled || !deployment.configured) && <p className="text-sm text-muted-foreground" role="status">{t('chatgptWeb.serviceNotReady')}</p>}
    <Button type="button" variant="outline" size="sm" disabled={busy} onClick={() => void load()}>{t('chatgptWeb.refresh')}</Button>
    {error && <p role="alert" className="break-words text-sm text-destructive">{error}</p>}
    <section className="min-w-0 space-y-3 rounded-md border p-3" aria-label={t('chatgptWeb.batchTitle')} aria-busy={busy}>
      <h4 className="text-sm font-medium">{t('chatgptWeb.batchTitle')}</h4>
      <p className="text-xs text-muted-foreground">{t('chatgptWeb.batchHint')}</p>
      <label className="block space-y-1 text-sm"><span>{t('chatgptWeb.batchScope')}</span><select aria-label={t('chatgptWeb.batchScope')} className="h-9 w-full min-w-0 rounded-md border bg-background px-3" value={scope} disabled={busy} onChange={event => { setScope(event.target.value as ChatGPTWebBatchRequest['scope']); setSummary(null) }}>
        <option value="accounts">{t('chatgptWeb.scopeAccounts')}</option><option value="group">{t('chatgptWeb.scopeGroup')}</option><option value="api_key">{t('chatgptWeb.scopeKey')}</option>
      </select></label>
      {scope === 'accounts' ? <fieldset className="max-h-48 space-y-1 overflow-auto" disabled={busy}>
        <legend className="text-xs text-muted-foreground">{t('chatgptWeb.selectAccounts', { count: targetIDs.length })}</legend>
        {eligibleAccounts.map(account => <label key={account.id} className="flex min-w-0 items-start gap-2 text-sm"><input type="checkbox" className="mt-1 shrink-0" checked={selected.includes(account.id)} onChange={event => { setSelected(old => event.target.checked ? [...old, account.id] : old.filter(id => id !== account.id)); setSummary(null) }} /><span className="min-w-0 break-words">#{account.id} {account.name || account.email}</span></label>)}
      </fieldset> : <label className="block space-y-1 text-sm"><span>{t(scope === 'group' ? 'chatgptWeb.scopeGroup' : 'chatgptWeb.scopeKey')}</span><select aria-label={t(scope === 'group' ? 'chatgptWeb.scopeGroup' : 'chatgptWeb.scopeKey')} className="h-9 w-full min-w-0 rounded-md border bg-background px-3" value={scope === 'group' ? groupID : keyID} disabled={busy} onChange={event => { if (scope === 'group') setGroupID(event.target.value); else setKeyID(event.target.value); setSummary(null) }}>
        <option value="">{t('chatgptWeb.chooseScope')}</option>{(scope === 'group' ? groups : keys).map(item => <option key={item.id} value={item.id}>#{item.id} {item.name}</option>)}
      </select></label>}
      <div className="flex flex-wrap gap-2"><Button type="button" size="sm" disabled={busy || !validScope} onClick={() => void batchAction(true)}>{t('chatgptWeb.batchEnable')}</Button><Button type="button" variant="outline" size="sm" disabled={busy || !validScope} onClick={() => void batchAction(false)}>{t('chatgptWeb.batchDisable')}</Button></div>
      {summary && <div className="space-y-1 break-words text-sm" role="status" data-testid="web-batch-summary"><p>{t('chatgptWeb.batchSummary', { ...summary })}</p>{summary.results.filter(item => item.code || item.error).map(item => <p key={item.account_id}>#{item.account_id} · {item.code || '—'} · {item.error || '—'}</p>)}</div>}
    </section>
    <div className="max-h-96 space-y-2 overflow-auto">
      {eligibleAccounts.map(account => {
        const state = states.find(s => s.account_id === account.id)
        return <div key={account.id} className="min-w-0 space-y-2 break-words rounded-md border p-3 text-sm" data-web-account={account.id}>
          <p className="font-medium">{t('chatgptWeb.account')} #{account.id} {account.name || account.email}</p>
          <ChatGPTWebAccountBadge accountID={account.id} accountName={account.name || account.email} state={state ?? null} onStateChange={next => updateState(account.id, next)} />
          <p>{t('chatgptWeb.status')}: {t(webAccountStatusMessage(state ?? null))} · {t(state?.enabled ? 'chatgptWeb.enabled' : 'chatgptWeb.disabled')}</p>
          <p>{t('chatgptWeb.capabilities')}: {state?.capabilities?.join(', ') || t('chatgptWeb.unknown')} · {t('chatgptWeb.quota')}: {state?.image_quota_remaining ?? t('chatgptWeb.unknown')}</p>
          <p>{t('chatgptWeb.reset')}: {state?.quota_reset_at || '—'} · {t('chatgptWeb.observed')}: {state?.observed_at || '—'}</p>
          <p className="text-xs">{t('chatgptWeb.lastCheck')}: {state?.last_check_at || '—'} · {t('chatgptWeb.checkStage')}: {state?.last_check_stage || '—'} · {t('chatgptWeb.error')}: {state?.last_check_error_code || '—'} · {t('chatgptWeb.upstreamStatus')}: {state?.last_check_http_status || '—'}</p>
          <div className="flex flex-wrap gap-2"><Button type="button" variant="outline" size="sm" disabled={busy} onClick={() => void action(account.id, !state?.enabled, false)}>{t(state?.enabled ? 'chatgptWeb.disable' : 'chatgptWeb.enable')}</Button><Button type="button" variant="outline" size="sm" disabled={busy || !state?.enabled} onClick={() => void action(account.id, true, true)}>{t('chatgptWeb.check')}</Button></div>
        </div>
      })}
    </div>
    <h4 className="font-medium">{t('chatgptWeb.executions')}</h4>
    <div className="max-h-64 space-y-1 overflow-auto text-xs">{executions.length ? executions.map(e => <p key={e.id}>#{e.account_id} · {e.operation} · {e.stage} · {e.error_code || '—'} · {e.updated_at} {e.result_unknown && t('chatgptWeb.resultUnknown')}</p>) : t('chatgptWeb.empty')}</div>
  </CardContent></Card>
}
