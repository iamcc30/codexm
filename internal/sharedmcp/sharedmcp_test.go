package sharedmcp

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestSyncInheritsAndPreservesLocalConfiguration(t *testing.T) {
	dir := t.TempDir()
	shared := filepath.Join(dir, "shared.toml")
	profile := filepath.Join(dir, "profile.toml")
	sharedData := `[mcp_servers.alpha]
command = "alpha-server"

[mcp_servers.beta]
url = "https://shared.example/mcp"
`
	profileData := `# keep this comment
model = "gpt-test"

[mcp_servers.beta]
url = "https://local.example/mcp"
`
	mustWrite(t, shared, sharedData)
	mustWrite(t, profile, profileData)

	result, err := Sync(shared, profile, nil, true)
	if err != nil {
		t.Fatal(err)
	}
	if !result.Changed || strings.Join(result.Inherited, ",") != "alpha" || strings.Join(result.LocalOverrides, ",") != "beta" {
		t.Fatalf("unexpected result: %+v", result)
	}
	data := mustRead(t, profile)
	for _, want := range []string{"# keep this comment", `model = "gpt-test"`, "[mcp_servers.beta]", `url = "https://local.example/mcp"`, beginMarker, "[mcp_servers.alpha]", endMarker} {
		if !strings.Contains(data, want) {
			t.Fatalf("missing %q in:\n%s", want, data)
		}
	}
	if strings.Count(data, "[mcp_servers.beta]") != 1 {
		t.Fatalf("local override was duplicated:\n%s", data)
	}

	second, err := Sync(shared, profile, nil, true)
	if err != nil {
		t.Fatal(err)
	}
	if second.Changed {
		t.Fatalf("second sync should be idempotent: %+v", second)
	}
}

func TestSyncHonorsExclusionsAndRemovesDeletedServers(t *testing.T) {
	dir := t.TempDir()
	shared := filepath.Join(dir, "shared.toml")
	profile := filepath.Join(dir, "profile.toml")
	mustWrite(t, shared, "[mcp_servers.alpha]\ncommand = \"alpha\"\n\n[mcp_servers.beta]\ncommand = \"beta\"\n")
	mustWrite(t, profile, "model = \"gpt-test\"\n")

	result, err := Sync(shared, profile, []string{"beta"}, true)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Join(result.Inherited, ",") != "alpha" || strings.Join(result.Excluded, ",") != "beta" {
		t.Fatalf("unexpected result: %+v", result)
	}
	if strings.Contains(mustRead(t, profile), "mcp_servers.beta") {
		t.Fatal("excluded server was synchronized")
	}

	mustWrite(t, shared, "")
	result, err = Sync(shared, profile, nil, true)
	if err != nil {
		t.Fatal(err)
	}
	data := mustRead(t, profile)
	if !result.Changed || strings.Contains(data, beginMarker) || strings.Contains(data, "mcp_servers.alpha") {
		t.Fatalf("deleted shared servers were not removed:\n%s", data)
	}
	if !strings.Contains(data, `model = "gpt-test"`) {
		t.Fatalf("local config was lost:\n%s", data)
	}
}

func TestSyncRejectsInvalidManagedBlock(t *testing.T) {
	dir := t.TempDir()
	shared := filepath.Join(dir, "shared.toml")
	profile := filepath.Join(dir, "profile.toml")
	mustWrite(t, shared, "[mcp_servers.alpha]\ncommand = \"alpha\"\n")
	mustWrite(t, profile, "model = \"gpt-test\"\n"+beginMarker+"\n")

	if _, err := Sync(shared, profile, nil, true); err == nil || !strings.Contains(err.Error(), "unmatched marker") {
		t.Fatalf("expected unmatched marker error, got %v", err)
	}
}

func TestSyncRejectsInlineMCPTableThatCannotBeExtended(t *testing.T) {
	dir := t.TempDir()
	shared := filepath.Join(dir, "shared.toml")
	profile := filepath.Join(dir, "profile.toml")
	mustWrite(t, shared, "[mcp_servers.alpha]\ncommand = \"alpha\"\n")
	mustWrite(t, profile, "mcp_servers = { local = { command = \"local\" } }\n")

	if _, err := Sync(shared, profile, nil, true); err == nil || !strings.Contains(err.Error(), "cannot merge") {
		t.Fatalf("expected merge error, got %v", err)
	}
}

func TestServerNamesMissingFileIsEmpty(t *testing.T) {
	names, err := ServerNames(filepath.Join(t.TempDir(), "missing.toml"))
	if err != nil {
		t.Fatal(err)
	}
	if len(names) != 0 {
		t.Fatalf("expected no servers, got %v", names)
	}
}

func TestSyncWithoutSharedServersDoesNotTouchUnmanagedProfile(t *testing.T) {
	dir := t.TempDir()
	shared := filepath.Join(dir, "missing-shared.toml")
	profile := filepath.Join(dir, "profile.toml")
	original := "# preserve formatting\r\nmodel = \"gpt-test\"\r\n\r\n"
	mustWrite(t, profile, original)

	result, err := Sync(shared, profile, nil, true)
	if err != nil {
		t.Fatal(err)
	}
	if result.Changed {
		t.Fatal("profile without managed servers should not change")
	}
	if got := mustRead(t, profile); got != original {
		t.Fatalf("profile changed:\nwant %q\n got %q", original, got)
	}
}

func mustWrite(t *testing.T, path, content string) {
	t.Helper()
	if err := os.WriteFile(path, []byte(content), 0o600); err != nil {
		t.Fatal(err)
	}
}

func mustRead(t *testing.T, path string) string {
	t.Helper()
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	return string(data)
}
