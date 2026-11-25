import type { AccountRow } from '../types'

type Display = NonNullable<AccountRow['codex_ua_display']>
type Translate = (key: string, options?: Record<string, string>) => string

export function codexUADisplayText(display: Display, t: Translate) {
  const os: Record<string, string> = { macos: 'macOS', windows: 'Windows', linux: 'Linux' }
  const kinds: Record<string, string> = { 'codex-vscode': 'codexUAKindVscode', 'codex-desktop': 'codexUAKindDesktop', 'codex-tui': 'codexUAKindTui', 'codex-exec': 'codexUAKindExec', custom: 'codexUAKindCustom' }
  const msg = (key: string, options?: Record<string, string>) => t(`accounts.uaDisplay.${key}`, options)
  const kind = kinds[display.client] ? t(`settings.${kinds[display.client]}`) : msg('unknown')
  const system = os[display.system] ?? msg('unknown')
  const describe = (value: string, source: string) => {
    if (source === 'request') return msg('request')
    if (source === 'unknown') return msg('unknown')
    if (['oauth', 'preset', 'manual', 'global', 'pool'].includes(source)) return msg(source, { value })
    return value
  }
  return { system: describe(system, display.system_source), client: describe(kind, display.client_source) }
}
