package webasset

import (
	"strings"
	"testing"
)

func TestAssetReferenceBoundary(t *testing.T) {
	for _, value := range []string{"file_output", "sediment://file_00000000123456789abcdef012345678", strings.Repeat("a", 160)} {
		if !Valid(value) {
			t.Errorf("rejected valid reference %q", value)
		}
	}
	for _, value := range []string{"", "sediment://", "sediment://../file_x", "sediment://https://example.invalid", "https://chatgpt.com/file_x?secret=1", "file-service://file_x", "file/x", "file_x\n", "sediment://file_x?token=x", "sediment://file_x#x", "sediment://file_x%2fsecret", "sediment://file_x\\x", strings.Repeat("a", 161)} {
		if Valid(value) {
			t.Errorf("accepted unsafe reference %q", value)
		}
	}
}
