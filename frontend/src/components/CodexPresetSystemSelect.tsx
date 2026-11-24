import { useTranslation } from 'react-i18next'
import { bindCodexPreset, presetSystemOptions, readCodexPreset } from '../lib/codexPresetSystem'

export default function CodexPresetSystemSelect({ binding, onChange }: { binding: string; onChange: (binding: string) => void }) {
  const { t } = useTranslation()
  const { kind, system } = readCodexPreset(binding)
  return (
    <label className="inline-flex items-center gap-2 text-sm">
      {t('settings.codexUAPresetSystem')}
      <select className="h-9 rounded-lg border bg-background px-2 text-sm" value={system}
        onChange={event => onChange(bindCodexPreset(kind, event.target.value))}>
        <option value="">{t('settings.codexUAPresetSystemAuto')}</option>
        {presetSystemOptions(kind).map(option => <option key={option.value} value={option.value}>{option.label}</option>)}
      </select>
    </label>
  )
}
