package e2e_test

import "testing"

// TestLVMStorageContractFollowsSubstrateVersion keeps the n-1 upgrade able to
// install an older published substrate (DES-HOR-461-05).
func TestLVMStorageContractFollowsSubstrateVersion(t *testing.T) {
	for version, want := range map[string]int{"0.5.0": 2, "0.5.9": 2, "0.6.0": 3, "1.0.0": 3, "not-a-version": 3} {
		if got := len(lvmStorageContract(version).StorageClasses); got != want {
			t.Fatalf("substrate %s: %d classes, want %d", version, got, want)
		}
	}
}
