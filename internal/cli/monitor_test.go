package cli

import (
	"reflect"
	"testing"

	"github.com/iamcc30/codexm/internal/appserver"
)

func TestManagedRemoteSubcommandClassification(t *testing.T) {
	tests := []struct {
		args    []string
		managed bool
	}{
		{nil, true},
		{[]string{"hello"}, true},
		{[]string{"--model", "gpt", "resume", "--last"}, true},
		{[]string{"fork", "--last"}, true},
		{[]string{"archive", "id"}, true},
		{[]string{"exec", "hello"}, false},
		{[]string{"review", "--base", "main"}, false},
		{[]string{"mcp", "list"}, false},
	}
	for _, test := range tests {
		if got := supportsManagedRemote(test.args); got != test.managed {
			t.Errorf("supportsManagedRemote(%q) = %v, want %v", test.args, got, test.managed)
		}
	}
}

func TestCustomRemoteDetection(t *testing.T) {
	if !hasRemoteArgument([]string{"--remote", "ws://127.0.0.1:1"}) {
		t.Fatal("--remote was not detected")
	}
	if !hasRemoteArgument([]string{"--remote=unix://"}) {
		t.Fatal("--remote= was not detected")
	}
	if hasRemoteArgument([]string{"exec", "remote work"}) {
		t.Fatal("prompt text was mistaken for --remote")
	}
}

func TestManagedRemoteArgumentsUseEnvironmentTokenAndPreserveChildArgs(t *testing.T) {
	child := []string{"resume", "--last"}
	got := managedRemoteArgs("ws://127.0.0.1:4321", child)
	want := []string{
		"--remote", "ws://127.0.0.1:4321",
		"--remote-auth-token-env", appserver.RemoteTokenEnv,
		"resume", "--last",
	}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("managed args = %q, want %q", got, want)
	}
	for _, arg := range got {
		if arg == "secret-token" {
			t.Fatal("raw token was placed on the command line")
		}
	}
	if !reflect.DeepEqual(child, []string{"resume", "--last"}) {
		t.Fatalf("child arguments were mutated: %q", child)
	}
}
