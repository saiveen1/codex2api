import { useEffect, useRef, useState } from 'react'
import { useTranslation } from 'react-i18next'
import { api } from '../api'
import { useToast } from '../hooks/useToast'
import { formatAccountName } from '../lib/connectionTestModels'
import { preferredOneOffModel, submitAccountOneOffTest, type OneOffTestKind } from '../lib/accountOneOffTest'
import type { AccountRow } from '../types'
import { getErrorMessage } from '../utils/error'
import Modal from './Modal'
import { Button } from './ui/button'
import { Select } from './ui/select'

export default function AccountOneOffTestDialog({ account, kind, onClose }: {
  account: AccountRow; kind: OneOffTestKind; onClose: () => void
}) {
  const { t } = useTranslation()
  const { showToast } = useToast()
  const [options, setOptions] = useState<Awaited<ReturnType<typeof api.getQualityTestOptions>>>()
  const [model, setModel] = useState('')
  const [error, setError] = useState('')
  const [submitting, setSubmitting] = useState(false)
  const busy = useRef(false)
  const intelligence = kind === 'intelligence'
  const preset = t(intelligence ? 'accounts.onceCandy' : 'qualityTest.presets.builtins.pelican')

  useEffect(() => {
    const controller = new AbortController()
    api.getQualityTestOptions(account.id, controller.signal).then(result => {
      if (controller.signal.aborted) return
      setOptions(result)
      setModel(result.models.includes(preferredOneOffModel) ? preferredOneOffModel : '')
    }).catch(e => { if (!controller.signal.aborted) setError(getErrorMessage(e)) })
    return () => controller.abort()
  }, [account.id])

  async function submit() {
    if (busy.current || !options?.models.includes(model)) return
    busy.current = true
    setSubmitting(true)
    setError('')
    try {
      const id = await submitAccountOneOffTest(account.id, kind, model, options.reasoning_efforts, preset)
      showToast(t(intelligence ? 'accounts.onceIntelligenceStarted' : 'accounts.onceQualityStarted', { id }))
      onClose()
    } catch (e) { setError(getErrorMessage(e)) }
    finally { busy.current = false; setSubmitting(false) }
  }

  return <Modal show title={t(intelligence ? 'accounts.onceIntelligence' : 'accounts.onceQuality')}
    onClose={() => { if (!busy.current) onClose() }} showCloseButton={!submitting}
    footer={<>
      <Button variant="outline" disabled={submitting} onClick={onClose}>{t('common.cancel')}</Button>
      <Button disabled={!model || submitting} onClick={() => void submit()}>{t(submitting ? 'common.loading' : 'qualityTest.start')}</Button>
    </>}>
    <div className="space-y-4 text-sm">
      <div><p className="break-all font-semibold">{formatAccountName(account)} <span className="text-muted-foreground">#{account.id}</span></p>
        <p className="text-muted-foreground">{t('qualityTest.filters.preset')}: {preset}</p></div>
      <div className="space-y-1.5">
        <label htmlFor="once-test-model">{t('qualityTest.model')}</label>
        <Select id="once-test-model" value={model} onValueChange={setModel}
          options={(options?.models ?? []).map(value => ({ value, label: value }))} disabled={!options?.models.length || submitting}
          placeholder={t(!options && !error ? 'qualityTest.loadingModels' : 'accounts.onceChooseModel')} />
        {options?.models.length === 0 && <p className="text-muted-foreground">{t('qualityTest.noModels')}</p>}
      </div>
      <p className="text-muted-foreground">{t('qualityTest.runHint')}</p>
      {error && <p role="alert" className="text-destructive">{error}</p>}
    </div>
  </Modal>
}
