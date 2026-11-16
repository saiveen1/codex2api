export const WEB_IMAGE_SIZES = ['auto', '1:1', '3:2', '2:3', '3:4', '4:3', '16:9', '9:16']

export function imageKeyMatchesSource(source: 'default' | 'web', keyID: string, channel: string | undefined, found: boolean): boolean {
  if (!keyID) return source === 'default'
  return found && (source === 'web' ? channel === 'chatgpt_web' : channel !== 'chatgpt_web')
}

export function webImageParams<T extends { model?: string; size?: string; quality?: string; background?: string; output_format?: string }>(params: T): T {
  return { ...params, model: 'gpt-image-2', size: WEB_IMAGE_SIZES.includes(params.size || 'auto') ? (params.size || 'auto') : 'auto', quality: 'auto', background: 'auto', output_format: ['png', 'jpeg', 'webp'].includes(params.output_format || 'png') ? (params.output_format || 'png') : 'png' }
}

export function imageReferenceLimit(isWeb: boolean, nativeLimit = 4): number { return isWeb ? 2 : nativeLimit }
export function portalChannelReady(activeKey: string, snapshotKey?: string): boolean { return Boolean(activeKey && activeKey === snapshotKey) }
