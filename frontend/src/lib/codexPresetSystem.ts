const systems = { macos: 'macOS', windows: 'Windows', linux: 'Linux' } as const
export type CodexPresetSystem = keyof typeof systems

export function presetSystemOptions(kind: string) {
  if (!['codex-tui', 'codex-exec', 'codex-vscode', 'codex-desktop'].includes(kind)) return []
  return (Object.keys(systems) as CodexPresetSystem[])
    .filter(os => kind !== 'codex-desktop' || os !== 'linux')
    .map(value => ({ value, label: systems[value] }))
}

export function readCodexPreset(binding = '') {
  const [kind, system = ''] = binding.startsWith('raw:') ? ['raw:'] : binding.split('@')
  return { kind, system, systemLabel: systems[system as CodexPresetSystem] ?? '' }
}

export function bindCodexPreset(kind: string, system: string) {
  return kind && presetSystemOptions(kind).some(option => option.value === system) ? `${kind}@${system}` : kind
}
