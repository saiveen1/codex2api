export const WEB_IMAGE_SIZES = ['auto', '1:1', '3:2', '2:3', '3:4', '4:3', '16:9', '9:16']

export const WEB_IMAGE_MODELS = ['gpt-image-2', 'gpt-image-2.5', 'gpt-image-2.5-flare', 'gpt-image-2.5-sunburst', 'gpt-image-2.6', 'gpt-image-2.6-flare', 'gpt-image-2.6-sunburst']

export function webImageModelOptions(limits?: { model_allow?: string[]; model_deny?: string[] }) {
  if (!limits) return []
  const allow = (limits.model_allow || []).map(value => value.trim().toLowerCase())
  const deny = (limits.model_deny || []).map(value => value.trim().toLowerCase())
  return WEB_IMAGE_MODELS.filter(model => allow.length ? allow.includes(model) : !deny.includes(model)).map(value => ({ label: value, value }))
}

export function imageKeyMatchesSource(source: 'default' | 'web', keyID: string, channel: string | undefined, found: boolean): boolean {
  if (!keyID) return source === 'default'
  return found && (source === 'web' ? channel === 'chatgpt_web' : channel !== 'chatgpt_web')
}

export function webImageParams<T extends { model?: string; size?: string; quality?: string; background?: string; output_format?: string }>(params: T): T {
  return { ...params, size: WEB_IMAGE_SIZES.includes(params.size || 'auto') ? (params.size || 'auto') : 'auto', quality: 'auto', background: 'auto', output_format: ['png', 'jpeg', 'webp'].includes(params.output_format || 'png') ? (params.output_format || 'png') : 'png' }
}

export function imageReferenceLimit(isWeb: boolean, nativeLimit = 4): number { return isWeb ? 16 : nativeLimit }
export function portalChannelReady(activeKey: string, snapshotKey?: string): boolean { return Boolean(activeKey && activeKey === snapshotKey) }
