import { useTranslation } from 'react-i18next'
import { Activity, AlertTriangle, Brain, DatabaseZap, Image as ImageIcon, RefreshCw, Zap } from 'lucide-react'
import { Card, CardContent } from '@/components/ui/card'
import { cn } from '../lib/utils'
import { formatUsageNumber as formatTokens } from '../lib/usageFormat'
import { formatSharePercent } from '../lib/usageInsights'
import type { UsageFeatureStats } from '../types'
import '../pages/usage-insights.css'

const PANEL_ACCENTS = {
  cyan: 'bg-cyan-500/10 text-cyan-700 dark:text-cyan-300',
  violet: 'bg-violet-500/10 text-violet-600 dark:text-violet-300',
  blue: 'bg-primary/10 text-primary',
  amber: 'bg-amber-500/10 text-amber-700 dark:text-amber-300',
}

function safeNumber(value?: number | null): number {
  return typeof value === 'number' && Number.isFinite(value) ? value : 0
}

export default function FeatureStatsPanel({ stats, totalRequests, showFullUsageNumbers }: {
  stats?: UsageFeatureStats
  totalRequests: number
  showFullUsageNumbers: boolean
}) {
  const { t } = useTranslation()
  const stream = Math.max(0, safeNumber(stats?.stream_requests))
  const sync = Math.max(0, safeNumber(stats?.sync_requests))
  const share = (value: number) => totalRequests > 0 ? Math.max(0, Math.min(100, value / totalRequests * 100)) : 0
  const features = [
    { key: 'featureCache', value: stats?.cache_hit_requests, icon: <DatabaseZap />, accent: 'cyan' },
    { key: 'featureReasoning', value: stats?.reasoning_requests, icon: <Brain />, accent: 'violet' },
    { key: 'featureFast', value: stats?.fast_requests, icon: <Zap />, accent: 'blue' },
    { key: 'featureImage', value: stats?.image_requests, icon: <ImageIcon />, accent: 'amber' },
  ] as const

  return (
    <Card className="usage-insight-panel usage-feature-panel"><CardContent className="usage-insight-content">
      <div className="usage-panel-header">
        <div className={cn('usage-panel-icon', PANEL_ACCENTS.cyan)} aria-hidden="true"><Activity /></div>
        <div className="usage-panel-title"><h3>{t('usage.featureStatsTitle')}</h3><p title={t('usage.featureStatsDesc')}>{t('usage.featureStatsDesc')}</p></div>
      </div>
      <div className="usage-transport">
        <div className="usage-transport-values">
          {[{ key: 'featureStream', value: stream }, { key: 'featureSync', value: sync }].map((item) => (
            <div key={item.key}>
              <span className="usage-transport-label"><i aria-hidden="true" />{t(`usage.${item.key}`)}</span>
              <div><strong>{formatTokens(item.value, showFullUsageNumbers)}</strong><span>{formatSharePercent(share(item.value))}</span></div>
            </div>
          ))}
        </div>
        <div className="usage-transport-track" aria-hidden="true">
          <span style={{ width: `${share(stream)}%` }} />
          <span style={{ width: `${share(sync)}%` }} />
        </div>
      </div>
      <div className="usage-feature-heading"><span className="usage-eyebrow">{t('usage.featureTraits')}</span><span>{t('usage.featureOverlapHint')}</span></div>
      <div className="usage-feature-grid">
        {features.map((item) => {
          const value = Math.max(0, safeNumber(item.value))
          return (
            <div key={item.key} className="usage-feature-item">
              <span aria-hidden="true" className={cn('usage-feature-icon', PANEL_ACCENTS[item.accent])}>{item.icon}</span>
              <div className="usage-feature-content">
                <span>{t(`usage.${item.key}`)}</span>
                <div><strong>{formatTokens(value, showFullUsageNumbers)}</strong><span>{formatSharePercent(share(value))}</span></div>
              </div>
            </div>
          )
        })}
      </div>
      <div className="usage-panel-footer usage-status-strip">
        {[{ key: 'featureRetry', value: stats?.retry_requests, icon: <RefreshCw />, tone: 'text-amber-600 dark:text-amber-400' }, { key: 'featureError', value: stats?.error_requests, icon: <AlertTriangle />, tone: 'text-rose-600 dark:text-rose-400' }].map((item) => {
          const value = Math.max(0, safeNumber(item.value))
          return (
            <div key={item.key} className={value > 0 ? item.tone : undefined}>
              <span className="usage-status-label"><span aria-hidden="true">{item.icon}</span>{t(`usage.${item.key}`)}</span>
              <strong>{formatTokens(value, showFullUsageNumbers)}</strong><span>{formatSharePercent(share(value))}</span>
            </div>
          )
        })}
      </div>
    </CardContent></Card>
  )
}
