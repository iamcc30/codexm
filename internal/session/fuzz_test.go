package session

import (
	"encoding/json"
	"path/filepath"
	"strings"
	"testing"
)

func FuzzDecodeObject(f *testing.F) {
	for _, seed := range []string{
		`{"type":"session_meta","payload":{"id":"11111111-1111-4111-8111-111111111111"}}`,
		`{}`,
		`{"number":9007199254740993}`,
		`[]`,
		`{} {}`,
	} {
		f.Add(seed)
	}
	f.Fuzz(func(t *testing.T, input string) {
		value, err := decodeObject([]byte(input))
		if err != nil {
			return
		}
		encoded, err := json.Marshal(value)
		if err != nil {
			t.Fatal(err)
		}
		if _, err := decodeObject(encoded); err != nil {
			t.Fatalf("successful decode did not round trip: %v", err)
		}
	})
}

func FuzzSafeRelative(f *testing.F) {
	for _, seed := range []string{"sessions/a.jsonl", "../outside", "/absolute", `C:\\absolute`, ".", ""} {
		f.Add(seed)
	}
	f.Fuzz(func(t *testing.T, input string) {
		if !safeRelative(input) {
			return
		}
		clean := filepath.Clean(filepath.FromSlash(input))
		portable := strings.ReplaceAll(input, `\`, "/")
		if filepath.IsAbs(clean) || windowsDriveAbsolute(portable) || clean == ".." || strings.HasPrefix(clean, ".."+string(filepath.Separator)) || strings.HasPrefix(portable, "../") {
			t.Fatalf("unsafe path accepted: %q -> %q", input, clean)
		}
	})
}
