package config

import "testing"

func FuzzValidateProfileName(f *testing.F) {
	for _, seed := range []string{"work", "account.one", ".", "..", "../escape", "中文", ""} {
		f.Add(seed)
	}
	f.Fuzz(func(t *testing.T, name string) {
		if ValidateProfileName(name) != nil {
			return
		}
		if name == "" || name == "." || name == ".." {
			t.Fatalf("unsafe profile name accepted: %q", name)
		}
		for _, r := range name {
			allowed := r >= 'a' && r <= 'z' || r >= 'A' && r <= 'Z' || r >= '0' && r <= '9' || r == '-' || r == '_' || r == '.'
			if !allowed {
				t.Fatalf("unexpected rune %q accepted in %q", r, name)
			}
		}
	})
}
