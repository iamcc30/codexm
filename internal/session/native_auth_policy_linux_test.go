package session

import (
	"os"
	"path/filepath"
	"testing"
)

func TestNativeAuthPolicyRejectsManagedAndUnknownSources(t *testing.T) {
	if err := nativeAuthHasNoManagedPolicy([]byte(`{"auth_mode":"apikey","OPENAI_API_KEY":"synthetic-not-a-secret"}`)); err != nil {
		t.Fatal(err)
	}
	for _, data := range []string{`{}`, `{"auth_mode":"personalAccessToken"}`, `{"auth_mode":"chatgpt","tokens":{"id_token":"personal-plan-can-refresh"}}`, `{"OPENAI_API_KEY":"synthetic","tokens":{}}`, `{"agent_identity":{}}`} {
		if err := nativeAuthHasNoManagedPolicy([]byte(data)); err == nil {
			t.Fatal("unresolved or refreshable auth source accepted")
		}
	}
	home := t.TempDir()
	t.Setenv("OPENAI_API_KEY", "")
	if _, err := nativeAuthPolicy(home); err == nil {
		t.Fatal("unclassified interactive login route accepted")
	}
	config := []byte("model_provider='local'\n[model_providers.local]\nrequires_openai_auth=false\n")
	if err := os.WriteFile(filepath.Join(home, "config.toml"), config, 0600); err != nil {
		t.Fatal(err)
	}
	if _, err := nativeAuthPolicy(home); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(home, "cloud-config-bundle-cache.json"), []byte("synthetic-policy"), 0600); err != nil {
		t.Fatal(err)
	}
	if _, err := nativeAuthPolicy(home); err == nil {
		t.Fatal("cloud policy accepted")
	}
	if err := os.Remove(filepath.Join(home, "cloud-config-bundle-cache.json")); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(home, "config.toml"), []byte("cli_auth_credentials_store='keyring'\n"), 0600); err != nil {
		t.Fatal(err)
	}
	if _, err := nativeAuthPolicy(home); err == nil {
		t.Fatal("uninspected keyring credentials accepted")
	}
}
