package session

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
)

// Keep local synchronization artifacts out of the portable mirror. The lock
// itself must remain in place after use; removing it could split lock ownership.
func ensureLocalIgnores(root string) error {
	path := filepath.Join(root, projectDirName, ".gitignore")
	if err := rejectSymlink(path); err != nil {
		return err
	}
	data, err := os.ReadFile(path)
	if err != nil && !errors.Is(err, os.ErrNotExist) {
		return err
	}
	const begin = "# BEGIN codexm local synchronization files"
	const end = "# END codexm local synchronization files"
	block := begin + "\n/session-writer.lock\n/pending-transaction.json\n/.codexm-transaction-validate-*/\n**/.*.tmp-*\n" + end
	text := string(data)
	i, j := strings.Index(text, begin), strings.Index(text, end)
	if (i >= 0) != (j >= 0) || (i >= 0 && j < i) {
		return fmt.Errorf("invalid managed block in %s", path)
	}
	if i >= 0 {
		text = text[:i] + block + text[j+len(end):]
	} else {
		if text != "" && !strings.HasSuffix(text, "\n") {
			text += "\n"
		}
		text += block + "\n"
	}
	if text == string(data) {
		return nil
	}
	return atomicWrite(path, []byte(text), 0o644)
}
