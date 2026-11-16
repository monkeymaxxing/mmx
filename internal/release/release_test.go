package release

import (
	"errors"
	"io/fs"
	"os"
	"strings"
	"testing"
)

// The repository keeps the development version even when tests are stamped.
func TestRepositoryOfficialExtraModulesCarryDefaultVersion(t *testing.T) {
	b, err := os.ReadFile("../../official_extra_modules/shipped_with_mmx_version")
	if errors.Is(err, fs.ErrNotExist) {
		t.Skip("the test binary runs outside the repository, as on a test node")
	}
	if err != nil || strings.TrimSpace(string(b)) != DefaultVersion {
		t.Fatalf("official_extra_modules/shipped_with_mmx_version holds %q, want %s: %v", b, DefaultVersion, err)
	}
}
