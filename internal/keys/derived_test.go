package keys

import "testing"

// The derived keys must stay bit for bit what they are: chat and workspace keys already in LiteLLM (and in running notebooks) are recomputed, never stored. Values taken from the derivation as of 10.11.2026; do not change them.
func TestDerivedKeysGolden(t *testing.T) {
	for _, tc := range []struct{ got, want string }{
		{ChatKey("master-key", "kc-6f31baab"), "sk-lc-e584f746f04bc443f3adcf712dd7fe4032b592e38996718b"},
		{ChatKey("sk-1234", "kc-0b6e4a1c-2f3d-4e5f-8a9b-0c1d2e3f4a5b"), "sk-lc-e4e6d8c997e88cd97fb4822655bca580448266ae923aebfb"},
		{ChatKey("", ""), "sk-lc-171bd7a0aa97d6636bbaa33b37aa2f591e931cf0f8136db4"},
		{WorkspaceKey("master-key", "kc-6f31baab", "wwi23seb-ml"), "sk-ws-bb82de757be0f65ddffb798543aa4a41f773f7709262fb89"},
		{WorkspaceKey("master-key", "kc-6f31baab", "kurs"), "sk-ws-c31a4ac3b9516e850668f9f6485872b0c74c5e12d46067fb"},
		{WorkspaceKey("master-key", "kc-6f31baab", ""), "sk-ws-52f7f8327c662c25a3565643970139e599ea0e17827a4005"},
		{WorkspaceKey("sk-1234", "kc-0b6e4a1c-2f3d-4e5f-8a9b-0c1d2e3f4a5b", "wwi23seb-ml"), "sk-ws-43fcadd77dffa130650d8c6dcb206f8818e4b93ab51c2357"},
		{WorkspaceKey("sk-1234", "kc-0b6e4a1c-2f3d-4e5f-8a9b-0c1d2e3f4a5b", "kurs"), "sk-ws-958637c837f44eb4023dee2ff3eff63cf809e166f73cb672"},
		{WorkspaceKey("sk-1234", "kc-0b6e4a1c-2f3d-4e5f-8a9b-0c1d2e3f4a5b", ""), "sk-ws-49ada727e048f510cf36c1f3ad7ab63163b3b4d5c3497c6b"},
		{WorkspaceKey("", "", "wwi23seb-ml"), "sk-ws-a27d2f3ffa0460f20e004e39eb177b3370854935ffdc976c"},
		{WorkspaceKey("", "", "kurs"), "sk-ws-af9d6004df9ffa320322226038161a82f6ea976b49a33921"},
		{WorkspaceKey("", "", ""), "sk-ws-44ebf696642f1ac777998dfd1dfb3e8b121084a3ef82d514"},
	} {
		if tc.got != tc.want {
			t.Errorf("derived key %s, want %s", tc.got, tc.want)
		}
	}
}
