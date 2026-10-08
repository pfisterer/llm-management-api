package keys

import "testing"

// The expected value was computed with the former Node broker's derivation
// (crypto.createHmac("sha256", key).update("librechat:"+uid)); a mismatch would
// orphan every existing chat key.
func TestChatKeyMatchesBroker(t *testing.T) {
	if got := ChatKey("master-key", "kc-6f31baab"); got != "sk-lc-e584f746f04bc443f3adcf712dd7fe4032b592e38996718b" {
		t.Fatalf("ChatKey = %s", got)
	}
}

func TestEnsureChatKey(t *testing.T) {
	ctx := t.Context()
	svc, f := setup(t)
	p := Person{Subject: "abc", Email: "a@dhbw.de"}
	want := ChatKey("master", p.UserID())

	got, err := svc.EnsureChatKey(ctx, p, student, "master")
	if err != nil || got != want {
		t.Fatalf("first: %q %v", got, err)
	}
	// Second time: the key exists, nothing new is created.
	n := len(f.keys)
	if got, err = svc.EnsureChatKey(ctx, p, student, "master"); err != nil || got != want || len(f.keys) != n {
		t.Fatalf("second: %q %v keys=%d", got, err, len(f.keys))
	}
	// A new master key: new chat key, and the stale one is gone.
	if _, err = svc.EnsureChatKey(ctx, p, student, "rotated"); err != nil {
		t.Fatal(err)
	}
	chat := 0
	for _, k := range f.keys {
		if isChatAlias(k["key_alias"].(string)) {
			chat++
			if k["key"] != ChatKey("rotated", p.UserID()) {
				t.Fatalf("stale chat key survived: %v", k["key"])
			}
		}
	}
	if chat != 1 {
		t.Fatalf("chat keys: %d", chat)
	}
	// It does not count against the person's own keys.
	if _, _, err := svc.Create(ctx, p, student, "vscode"); err != nil {
		t.Fatalf("own key after chat key: %v", err)
	}
}
