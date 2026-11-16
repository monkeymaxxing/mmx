//go:build !mmx_owed_restore_test

package modules

import "testing"

func TestProductionRegistryExcludesOwedRestoreTestModule(t *testing.T) {
	t.Parallel()
	if _, found := LookUpCoreModule("test_change_with_owed_restore"); found {
		t.Fatal("test-only owed restore adapter is in the production registry")
	}
}
