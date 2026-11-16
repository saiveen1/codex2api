import { Link } from 'react-router-dom'
import { useTranslation } from 'react-i18next'
import type { ChatGPTWebState } from '../api'
import { webAccountStatus, webAccountStatusMessage } from '../lib/chatgptWebStatus'

export default function ChatGPTWebAccountBadge({ state }: { state?: ChatGPTWebState | null }) {
  const { t } = useTranslation()
  const status = webAccountStatus(state)
  const quota = state?.enabled && typeof state.image_quota_remaining === 'number' ? state.image_quota_remaining : null
  const title = [t(webAccountStatusMessage(state)), state?.observed_at && `${t('chatgptWeb.observed')}: ${state.observed_at}`].filter(Boolean).join('\n')
  return <Link to="/settings#settings-codex-images" title={title} className="inline-flex max-w-full shrink-0 items-center gap-1 rounded border border-border px-1.5 py-0.5 text-[11px] font-medium text-muted-foreground hover:text-foreground" onClick={event => event.stopPropagation()}>
    {t('chatgptWeb.accountBadge', { status: t(`chatgptWeb.states.${status}`) })}
    {quota !== null && <span>{t('chatgptWeb.quota')}: {quota}</span>}
  </Link>
}
