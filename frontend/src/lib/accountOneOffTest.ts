import { api } from '../api.ts'
import { PELICAN_PROMPT, QUALITY_TEST_DEFAULT_TIMEOUT_MINUTES } from './qualityTest.ts'

export type OneOffTestKind = 'intelligence' | 'quality'
export const preferredOneOffModel = 'gpt-6.2-sol'

export async function submitAccountOneOffTest(accountID: number, kind: OneOffTestKind, model: string, efforts: string[], presetName: string) {
  if (kind === 'intelligence') return (await api.createAccountIntelligenceTest(accountID, model)).run.id
  const { job } = await api.createQualityTest(accountID, {
    model, prompt: PELICAN_PROMPT, preset_key: 'pelican', preset_name: presetName,
    reasoning_effort: efforts.includes('high') ? 'high' : '', timeout_minutes: QUALITY_TEST_DEFAULT_TIMEOUT_MINUTES,
  })
  return job.id
}
