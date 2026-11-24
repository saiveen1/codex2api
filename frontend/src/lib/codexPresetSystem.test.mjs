import test from 'node:test'
import assert from 'node:assert/strict'
import { bindCodexPreset, presetSystemOptions, readCodexPreset } from './codexPresetSystem.ts'

test('client and OS round-trip independently, including returning to automatic', () => {
  for (const [kind, os, label] of [['codex-tui', 'macos', 'macOS'], ['codex-desktop', 'windows', 'Windows'], ['codex-exec', 'linux', 'Linux']]) {
    const binding = bindCodexPreset(kind, os)
    assert.equal(binding, `${kind}@${os}`)
    assert.deepEqual(readCodexPreset(binding), { kind, system: os, systemLabel: label })
    assert.equal(bindCodexPreset(kind, ''), kind)
    assert.equal(readCodexPreset(kind).system, '')
  }
})

test('raw, inherit and unsupported OS do not acquire a stale system suffix', () => {
  assert.deepEqual(presetSystemOptions('codex-desktop').map(option => option.value), ['macos', 'windows'])
  assert.equal(bindCodexPreset('codex-desktop', 'linux'), 'codex-desktop')
  assert.equal(bindCodexPreset('raw:', 'macos'), 'raw:')
  assert.equal(bindCodexPreset('', 'windows'), '')
  assert.equal(readCodexPreset('raw:Sample/1.2 @windows').system, '')
})
