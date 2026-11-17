// Package webasset validates opaque Worker asset references, never fetch URLs.
package webasset

import "strings"

// Valid accepts the two representations emitted by the Worker: a bare file ID
// or a sediment pointer. Preserve the pointer because it selects a different
// authenticated upstream resolver; never strip it into a colliding bare ID.
func Valid(value string) bool {
	bare := strings.TrimPrefix(value, "sediment://")
	if bare == "" || len(bare) > 160 {
		return false
	}
	for _, c := range bare {
		if !(c >= 'a' && c <= 'z' || c >= 'A' && c <= 'Z' || c >= '0' && c <= '9' || c == '_' || c == '-') {
			return false
		}
	}
	return true
}
