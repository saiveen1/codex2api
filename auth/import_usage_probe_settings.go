package auth

// ImportUsageProbeEnabled reports whether successful imports trigger a usage probe.
func (s *Store) ImportUsageProbeEnabled() bool {
	return s == nil || !s.importUsageProbeDisabled.Load()
}

// SetImportUsageProbeEnabled controls usage probes triggered by successful imports.
func (s *Store) SetImportUsageProbeEnabled(enabled bool) {
	s.importUsageProbeDisabled.Store(!enabled)
}
