import { useTranslation } from 'react-i18next'
import type { AccountRow } from '../types'
import { codexUADisplayText } from '../lib/codexUADisplay'

export default function AccountUAStatus({ display, detail = false }: { display: AccountRow['codex_ua_display']; detail?: boolean }) {
  const { t } = useTranslation()
  if (!display) return null
  const { system, client } = codexUADisplayText(display, t)
  return <div title={detail ? undefined : display.user_agent} className={detail ? 'my-2 rounded-lg bg-muted/50 px-3 py-2 text-xs' : 'text-[11px] leading-snug text-muted-foreground'}>
    {detail && <div className="mb-1 font-medium">{t('accounts.uaDisplay.saved')}</div>}
    <div>{t('accounts.uaDisplay.system', { value: system })}</div>
    <div>{t('accounts.uaDisplay.client', { value: client })}</div>
    {detail && display.user_agent && <details className="mt-1">
      <summary className="cursor-pointer">{t('accounts.uaDisplay.full')}</summary>
      <code className="block break-all pt-1">{display.user_agent}</code>
    </details>}
  </div>
}
