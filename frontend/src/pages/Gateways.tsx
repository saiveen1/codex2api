import { lazy, Suspense, useEffect, useState } from 'react'
import { Link, Navigate, NavLink, useLocation, useParams } from 'react-router-dom'
import { useTranslation } from 'react-i18next'
import { PROXY_POOL_CHANGED } from '../api'
import PageHeader from '../components/PageHeader'
import StateShell from '../components/StateShell'

const ChatGPTWebSettings = lazy(() => import('../components/ChatGPTWebSettings'))
const OAuthGateway = lazy(() => import('./OAuthGateway'))
const ProxySubscriptions = lazy(() => import('../components/ProxySubscriptions'))

const tabs = [
  { to: '/gateways', label: 'gateways.web' },
  { to: '/gateways/oauth', label: 'gateways.oauth' },
  { to: '/gateways/subscriptions', label: 'gateways.subscriptions' },
] as const
const notifyProxyPoolChanged = () => window.dispatchEvent(new Event(PROXY_POOL_CHANGED))

export default function Gateways() {
  const { t } = useTranslation()
  const { tab = 'web' } = useParams()
  const { hash } = useLocation()
  const oauthActive = tab === 'oauth'
  const [hasOpenedOAuth, setHasOpenedOAuth] = useState(oauthActive)
  useEffect(() => {
    if (oauthActive) setHasOpenedOAuth(true)
  }, [oauthActive])
  useEffect(() => {
    if (!hash) window.scrollTo({ top: 0 })
  }, [tab, hash])

  if (tab !== 'web' && !oauthActive && tab !== 'subscriptions') return <Navigate to="/gateways" replace />

  return (
    <div className="min-w-0 text-left">
      <PageHeader title={t('nav.gateways')} description={t('gateways.description')} />
      <nav aria-label={t('nav.gateways')} className="mb-5 flex flex-wrap gap-x-2 border-b border-border">
        {tabs.map(({ to, label }) => (
          <NavLink key={to} to={to} end className="inline-flex min-h-11 items-center rounded-t-md border-b-2 border-transparent px-3 text-sm font-medium text-muted-foreground hover:bg-muted/40 hover:text-foreground focus-visible:outline-2 focus-visible:outline-ring aria-[current=page]:border-primary aria-[current=page]:text-primary">
            {t(label)}
          </NavLink>
        ))}
      </nav>
      <Suspense fallback={<StateShell loading>{null}</StateShell>}>
        {tab === 'web' && <ChatGPTWebSettings />}
        {/* Keep in-flight OAuth requests and job state alive when switching gateway tabs. */}
        {(oauthActive || hasOpenedOAuth) && (
          <div hidden={!oauthActive}>
            <OAuthGateway embedded active={oauthActive} />
          </div>
        )}
        {tab === 'subscriptions' && (
          <div className="space-y-3">
            <ProxySubscriptions onChanged={notifyProxyPoolChanged} />
            <Link to="/proxies" className="inline-block text-sm text-primary underline underline-offset-4">{t('nav.proxies')}</Link>
          </div>
        )}
      </Suspense>
    </div>
  )
}
