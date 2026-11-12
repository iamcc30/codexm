package session

import (
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"strings"

	"github.com/pelletier/go-toml/v2"
)

// OAuth refresh can change workspace policy after local account inspection.
// This offline adapter therefore accepts no refreshable/native token source.
func nativeAuthPolicy(home string) (string, error) {
	if _, exists, err := frozenReadFile(filepath.Join(home, "cloud-config-bundle-cache.json")); err != nil {
		return "", err
	} else if exists {
		return "", errors.New("cloud-managed native configuration is unsupported for offline history maintenance")
	}
	config, exists, err := frozenReadFile(filepath.Join(home, "config.toml"))
	if err != nil {
		return "", err
	}
	var values map[string]any
	if exists {
		if err := toml.Unmarshal(config, &values); err != nil {
			return "", errors.New("invalid native configuration")
		}
		if store, present := values["cli_auth_credentials_store"]; present && store != "file" {
			return "", errors.New("native-history maintenance requires file-based CLI credentials; dynamic credential sources are unsupported")
		}
	}
	auth, exists, err := frozenReadFile(filepath.Join(home, "auth.json"))
	if err != nil {
		return "", err
	}
	if !exists {
		if strings.TrimSpace(os.Getenv("OPENAI_API_KEY")) != "" {
			return "auth-file-absent-api-key-env", nil
		}
		provider, _ := values["model_provider"].(string)
		providers, _ := values["model_providers"].(map[string]any)
		selected, _ := providers[provider].(map[string]any)
		requires, present := selected["requires_openai_auth"].(bool)
		if provider != "" && present && !requires {
			return "auth-file-absent-explicit-auth-free-provider", nil
		}
		return "", errors.New("native-history maintenance requires an API-key profile or an explicit auth-free provider; ChatGPT/OAuth policy is unsupported")
	}
	if len(auth) > 1<<20 {
		return "", errors.New("native auth metadata exceeds the supported size")
	}
	if err := nativeAuthHasNoManagedPolicy(auth); err != nil {
		return "", err
	}
	// Only an opaque hash enters configuration evidence, never credentials.
	return "auth-file:" + checksumBytes(auth), nil
}

func nativeAuthHasNoManagedPolicy(data []byte) error {
	var raw map[string]json.RawMessage
	if err := json.Unmarshal(data, &raw); err != nil {
		return errors.New("invalid native auth metadata")
	}
	for key := range raw {
		switch key {
		case "auth_mode", "OPENAI_API_KEY", "tokens", "last_refresh":
		default:
			return errors.New("unsupported native auth metadata source")
		}
	}
	var auth struct {
		Mode   string          `json:"auth_mode"`
		Key    string          `json:"OPENAI_API_KEY"`
		Tokens json.RawMessage `json:"tokens"`
	}
	if err := json.Unmarshal(data, &auth); err != nil {
		return errors.New("unsupported native auth metadata format")
	}
	if len(auth.Tokens) > 0 && strings.TrimSpace(string(auth.Tokens)) != "null" {
		return errors.New("refreshable ChatGPT/OAuth credentials are unsupported for offline history maintenance")
	}
	if (auth.Mode == "" || auth.Mode == "apikey") && auth.Key != "" {
		return nil
	}
	return errors.New("native-history maintenance supports only file API-key credentials or explicit auth-free providers")
}
