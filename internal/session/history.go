package session

import (
	"bytes"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"time"
)

// HistoryFormatVersion is intentionally incompatible with readers that only
// understand standalone rollouts. A v1 reader must stop before syncing a graph.
const HistoryFormatVersion = 2

type HistoryPosition struct {
	ThreadID            string `json:"thread_id"`
	EndByteOffset       uint64 `json:"end_byte_offset"`
	EndOrdinalExclusive uint64 `json:"end_ordinal_exclusive"`
}

// Dependency describes a bounded, byte-exact decompressed physical rollout.
// RolloutPath is the original profile-relative path; SnapshotPath is its
// immutable content-addressed copy in the portable mirror.
type Dependency struct {
	RolloutID           string       `json:"rollout_id"`
	SessionID           string       `json:"session_id"`
	RolloutPath         string       `json:"rollout_path"`
	SnapshotPath        string       `json:"snapshot_path"`
	RawChecksum         string       `json:"raw_checksum"`
	ByteCount           uint64       `json:"byte_count"`
	EndOrdinalExclusive uint64       `json:"end_ordinal_exclusive"`
	CWDs                []CWDMapping `json:"cwd_mappings,omitempty"`
}

func unsignedField(value map[string]any, key string) (uint64, error) {
	number, ok := value[key].(json.Number)
	if !ok {
		return 0, fmt.Errorf("missing or invalid %s", key)
	}
	return strconv.ParseUint(string(number), 10, 64)
}

func parseHistoryMeta(value map[string]any) (string, *HistoryPosition, error) {
	payload, ok := value["payload"].(map[string]any)
	if !ok {
		return "", nil, fmt.Errorf("session_meta payload is missing")
	}
	mode := "legacy"
	if raw, exists := payload["history_mode"]; exists {
		var valid bool
		mode, valid = raw.(string)
		if !valid || (mode != "legacy" && mode != "paginated") {
			return "", nil, fmt.Errorf("unsupported history_mode")
		}
	}
	raw, exists := payload["history_base"]
	if !exists || raw == nil {
		return mode, nil, nil
	}
	if mode != "paginated" {
		return "", nil, fmt.Errorf("history_base requires paginated history_mode")
	}
	base, ok := raw.(map[string]any)
	if !ok {
		return "", nil, fmt.Errorf("invalid history_base")
	}
	id := stringValue(base["thread_id"])
	offset, offsetErr := unsignedField(base, "end_byte_offset")
	ordinal, ordinalErr := unsignedField(base, "end_ordinal_exclusive")
	if !validUUID(id) || offsetErr != nil || ordinalErr != nil || offset == 0 || ordinal == 0 {
		return "", nil, fmt.Errorf("invalid history_base identity, byte offset, or ordinal")
	}
	return mode, &HistoryPosition{ThreadID: id, EndByteOffset: offset, EndOrdinalExclusive: ordinal}, nil
}

func physicalRolloutID(path, logicalID string) (string, error) {
	name := filepath.Base(path)
	if strings.HasSuffix(name, ".zst") {
		name = strings.TrimSuffix(name, ".zst")
	}
	fail := func() (string, error) {
		return "", fmt.Errorf("rollout filename does not match session id or native format in %s", path)
	}
	if !strings.HasSuffix(name, ".jsonl") {
		return fail()
	}
	name = strings.TrimSuffix(name, ".jsonl")
	if !strings.HasPrefix(name, "rollout-") || len(name) < len("rollout-")+20 {
		return fail()
	}
	suffix := strings.TrimPrefix(name, "rollout-")
	if _, err := time.Parse("2006-01-02T15-04-05", suffix[:19]); err != nil || suffix[19] != '-' {
		return fail()
	}
	identities := strings.Split(suffix[20:], "_")
	if len(identities) < 1 || len(identities) > 2 || identities[0] != logicalID || !validUUID(identities[0]) {
		return fail()
	}
	if len(identities) == 1 {
		return logicalID, nil
	}
	if !validUUID(identities[1]) {
		return fail()
	}
	return identities[1], nil
}

func managedRolloutPath(path string) bool {
	if !safeRelative(path) {
		return false
	}
	clean := filepath.ToSlash(filepath.Clean(filepath.FromSlash(path)))
	return strings.HasPrefix(clean, "sessions/") || strings.HasPrefix(clean, "archived_sessions/")
}

func rolloutBytes(source *rollout) uint64 {
	var count uint64
	for _, line := range source.RawLines {
		count += uint64(len(line)) + 1
	}
	return count
}

func usesHistory(source *rollout) bool {
	return source.HistoryMode == "paginated" || source.RolloutID != source.ID
}

// attachHistory resolves only the source prefixes that a selected tip needs.
// A source's unrelated appended tail is neither parsed nor copied.
func attachHistory(tip *rollout, root string, catalog map[string]string) error {
	seen := map[string]bool{tip.RolloutID: true}
	var visit func(*rollout) error
	visit = func(child *rollout) error {
		if child.HistoryBase == nil {
			return nil
		}
		base := child.HistoryBase
		if seen[base.ThreadID] {
			return fmt.Errorf("history dependency cycle at rollout %s", base.ThreadID)
		}
		path, ok := catalog[base.ThreadID]
		if !ok {
			return fmt.Errorf("missing history source rollout %s for session %s", base.ThreadID, tip.ID)
		}
		seen[base.ThreadID] = true
		source, err := readRolloutPrefix(path, root, nil, base.EndByteOffset)
		if err != nil {
			return err
		}
		if source.HistoryMode != "paginated" || source.EndOrdinal != base.EndOrdinalExclusive {
			return fmt.Errorf("history ordinal boundary does not match source rollout %s", base.ThreadID)
		}
		source.Relative = filepath.ToSlash(path) // Caller replaces absolute path below.
		if err := visit(source); err != nil {
			return err
		}
		tip.Dependencies = append(tip.Dependencies, source)
		return nil
	}
	if err := visit(tip); err != nil {
		return err
	}
	return composeHistory(tip)
}

func composeHistory(tip *rollout) error {
	tip.Canonical = nil
	for _, dependency := range tip.Dependencies {
		tip.Canonical = append(tip.Canonical, dependency.LocalCanonical...)
	}
	tip.Canonical = append(tip.Canonical, tip.LocalCanonical...)
	tip.Checksum = checksumLines(tip.Canonical)
	return nil
}

func dependencyFor(source *rollout) Dependency {
	snapshot := filepath.ToSlash(filepath.Join("history", source.RolloutID, source.RawChecksum, filepath.Base(source.Relative)))
	return Dependency{RolloutID: source.RolloutID, SessionID: source.ID, RolloutPath: source.Relative,
		SnapshotPath: snapshot, RawChecksum: source.RawChecksum, ByteCount: rolloutBytes(source),
		EndOrdinalExclusive: source.EndOrdinal, CWDs: source.CWDs}
}

func metadataFor(source *rollout) Metadata {
	metadata := Metadata{FormatVersion: FormatVersion, SessionID: source.ID, RolloutPath: source.Relative,
		Checksum: source.Checksum, LineCount: source.LineCount, Archived: source.Archived, Name: source.Name, CWDs: source.CWDs}
	if usesHistory(source) {
		metadata.FormatVersion = HistoryFormatVersion
		metadata.RolloutID = source.RolloutID
		metadata.RawChecksum = source.RawChecksum
		for _, dependency := range source.Dependencies {
			metadata.Dependencies = append(metadata.Dependencies, dependencyFor(dependency))
		}
	}
	return metadata
}

func loadProjectHistory(root string, tip *rollout, metadata Metadata) error {
	if metadata.FormatVersion == FormatVersion {
		if tip.HistoryBase != nil || metadata.RolloutID != "" || metadata.RawChecksum != "" || len(metadata.Dependencies) != 0 {
			return fmt.Errorf("history rollout %s requires version 2 metadata", tip.ID)
		}
		return nil
	}
	if metadata.FormatVersion != HistoryFormatVersion || !usesHistory(tip) ||
		metadata.RolloutID != tip.RolloutID || metadata.RawChecksum != tip.RawChecksum {
		return fmt.Errorf("invalid history metadata for session %s", tip.ID)
	}
	byID := map[string]*rollout{}
	for _, item := range metadata.Dependencies {
		if !validUUID(item.RolloutID) || !validUUID(item.SessionID) || !managedRolloutPath(item.RolloutPath) ||
			!safeRelative(item.SnapshotPath) || item.ByteCount == 0 || item.EndOrdinalExclusive == 0 {
			return fmt.Errorf("invalid history dependency metadata for %s", tip.ID)
		}
		expected := filepath.ToSlash(filepath.Join("history", item.RolloutID, item.RawChecksum, filepath.Base(item.RolloutPath)))
		if item.SnapshotPath != expected || !validChecksum(item.RawChecksum) {
			return fmt.Errorf("invalid history snapshot path for %s", tip.ID)
		}
		source, err := readRollout(filepath.Join(root, projectDirName, filepath.FromSlash(item.SnapshotPath)), root, append([]CWDMapping{}, item.CWDs...))
		if err != nil {
			return err
		}
		if source.ID != item.SessionID || source.RolloutID != item.RolloutID || source.RawChecksum != item.RawChecksum ||
			source.HistoryMode != "paginated" || rolloutBytes(source) != item.ByteCount || source.EndOrdinal != item.EndOrdinalExclusive {
			return fmt.Errorf("history snapshot content does not match metadata for %s", tip.ID)
		}
		source.CWDs = item.CWDs
		source.Relative = item.RolloutPath
		if _, duplicate := byID[source.RolloutID]; duplicate {
			return fmt.Errorf("duplicate history dependency %s", source.RolloutID)
		}
		byID[source.RolloutID] = source
	}
	seen := map[string]bool{tip.RolloutID: true}
	var visit func(*rollout) error
	visit = func(child *rollout) error {
		if child.HistoryBase == nil {
			return nil
		}
		base := child.HistoryBase
		if seen[base.ThreadID] {
			return fmt.Errorf("history dependency cycle at rollout %s", base.ThreadID)
		}
		source := byID[base.ThreadID]
		if source == nil {
			return fmt.Errorf("missing history dependency %s", base.ThreadID)
		}
		if rolloutBytes(source) != base.EndByteOffset || source.EndOrdinal != base.EndOrdinalExclusive {
			return fmt.Errorf("history dependency boundary does not match rollout %s", base.ThreadID)
		}
		seen[base.ThreadID] = true
		if err := visit(source); err != nil {
			return err
		}
		tip.Dependencies = append(tip.Dependencies, source)
		return nil
	}
	if err := visit(tip); err != nil {
		return err
	}
	if len(tip.Dependencies) != len(byID) {
		return fmt.Errorf("unreferenced history snapshot in metadata for %s", tip.ID)
	}
	return composeHistory(tip)
}

func profileCatalog(home string) (map[string]string, map[string]rolloutHeader, error) {
	paths := map[string]string{}
	headers := map[string]rolloutHeader{}
	for _, dir := range []string{"sessions", "archived_sessions"} {
		files, err := regularFiles(filepath.Join(home, dir), isRolloutFile)
		if err != nil {
			return nil, nil, err
		}
		for _, path := range files {
			header, err := readRolloutHeader(path)
			if err != nil {
				return nil, nil, err
			}
			key := header.RolloutID
			if key == "" {
				key = path
			}
			if _, exists := paths[key]; exists {
				return nil, nil, fmt.Errorf("duplicate session id or physical rollout id %s in profile", header.RolloutID)
			}
			paths[key] = path
			headers[key] = header
		}
	}
	return paths, headers, nil
}

// preflightHistoryImport does not create any files. Existing physical sources
// are compared as raw bounded prefixes, and their later bytes are left alone.
func preflightHistoryImport(root, home string, source *rollout) (map[string]string, error) {
	if usesHistory(source) {
		if _, within := relativeWithin(root, source.InitialCWD); !within {
			return nil, fmt.Errorf("cannot relocate paginated session %s: its cwd is outside this checkout; inherited byte offsets require the original checkout path", source.ID)
		}
		// Mappings record the original checkout-relative cwd, including subdirectories.
		for _, mapping := range source.CWDs {
			value, err := decodeObject(source.RawLines[mapping.Line-1])
			if err != nil {
				return nil, err
			}
			payload, _ := value["payload"].(map[string]any)
			// Project discovery resolves symlinks and normalizes Windows case.
			// An alias of the same directory needs no JSONL byte rewrite.
			expected := filepath.Join(root, filepath.FromSlash(mapping.Path))
			if !sameHistoryCWD(expected, stringValue(payload["cwd"])) {
				return nil, fmt.Errorf("cannot relocate paginated session %s without changing history bytes", source.ID)
			}
		}
	}
	paths, headers, err := profileCatalog(home)
	if err != nil {
		return nil, err
	}
	retiring, err := profileRetirementPlan(home, source, paths, headers)
	if err != nil {
		return nil, err
	}
	if err := validateReferencedReplacement(root, source, paths, headers, retiring); err != nil {
		return nil, err
	}
	for _, dependency := range source.Dependencies {
		if path, exists := paths[dependency.RolloutID]; exists {
			prefix, err := readRolloutBounded(path, root, nil, rolloutBytes(dependency), true)
			if err != nil {
				return nil, err
			}
			if !sameRawPrefix(prefix.RawLines, dependency.RawLines) {
				return nil, fmt.Errorf("history source prefix conflict for rollout %s; existing source was not overwritten", dependency.RolloutID)
			}
		} else {
			target := filepath.Join(home, filepath.FromSlash(dependency.Relative))
			if err := ensureVacantRollout(target); err != nil {
				return nil, err
			}
		}
	}
	return paths, nil
}

// Existing directories must be the same filesystem object, even on Windows
// volumes that enable case-sensitive names. For missing historical cwd paths,
// retain only the previous exact-spelling allowance; do not guess aliases.
func sameHistoryCWD(expected, actual string) bool {
	expectedInfo, expectedErr := os.Stat(expected)
	actualInfo, actualErr := os.Stat(actual)
	if expectedErr == nil && actualErr == nil {
		return expectedInfo.IsDir() && actualInfo.IsDir() && os.SameFile(expectedInfo, actualInfo)
	}
	if !os.IsNotExist(expectedErr) || !os.IsNotExist(actualErr) {
		return false
	}
	return filepath.Clean(expected) == filepath.Clean(actual)
}

func ensureVacantRollout(path string) error {
	if err := rejectSymlink(path); err != nil {
		return err
	}
	if _, err := os.Stat(path); err == nil {
		return fmt.Errorf("history target already exists: %s", path)
	} else if !os.IsNotExist(err) {
		return err
	}
	return nil
}

func writeHistoryDependencies(root string, source *rollout) error {
	for _, dependency := range source.Dependencies {
		descriptor := dependencyFor(dependency)
		path := filepath.Join(root, projectDirName, filepath.FromSlash(descriptor.SnapshotPath))
		if err := writeRollout(path, dependency.RawLines, dependency.Compressed); err != nil {
			return err
		}
		// Persist every new history directory and snapshot rename before any
		// transaction can publish a sidecar that depends on these bytes.
		if err := projectStore(root).syncParents(path); err != nil {
			return err
		}
	}
	return nil
}

func importHistoryDependencies(home string, source *rollout, existing map[string]string) error {
	for _, dependency := range source.Dependencies {
		if err := materializeDependency(home, dependency, existing); err != nil {
			return err
		}
	}
	return nil
}

func historyPrefixConflict(left, right *rollout) bool {
	if (!usesHistory(left) && !usesHistory(right)) || (left.ProjectMeta.FormatVersion == FormatVersion && left.HistoryBase == nil && right.HistoryBase == nil && len(left.Dependencies) == 0 && len(right.Dependencies) == 0) {
		return false
	}
	originals := map[string]*rollout{}
	for _, source := range append(append([]*rollout{}, left.Dependencies...), left) {
		originals[source.RolloutID] = source
	}
	for _, source := range append(append([]*rollout{}, right.Dependencies...), right) {
		other := originals[source.RolloutID]
		if other == nil {
			continue
		}
		count := len(source.RawLines)
		if len(other.RawLines) < count {
			count = len(other.RawLines)
		}
		for index := 0; index < count; index++ {
			if !bytes.Equal(source.RawLines[index], other.RawLines[index]) {
				return true
			}
		}
	}
	return false
}

func sortedIDs[T any](values map[string]T) []string {
	ids := make([]string, 0, len(values))
	for id := range values {
		ids = append(ids, id)
	}
	sort.Strings(ids)
	return ids
}

// Upgrade the project gate before storing any history-dependent bytes. Older
// codexm versions reject project.json instead of partly syncing a v2 mirror.
func requireHistoryVersion(root string) error {
	path := filepath.Join(root, projectDirName, projectFileName)
	var project Project
	if err := readJSON(path, &project); err != nil {
		return err
	}
	if err := validateProject(project, path); err != nil {
		return err
	}
	if project.FormatVersion == HistoryFormatVersion {
		// A prior interrupted upgrade may have renamed this gate without
		// syncing its directory yet. Never publish v2 metadata ahead of it.
		return projectStore(root).syncParents(path)
	}
	project.FormatVersion = HistoryFormatVersion
	if err := atomicWriteJSON(path, project, 0o644); err != nil {
		return err
	}
	if err := projectStore(root).syncParents(path); err != nil {
		return err
	}
	attributes := filepath.Join(root, projectDirName, ".gitattributes")
	if err := ensureAttributes(attributes); err != nil {
		return err
	}
	return projectStore(root).syncParents(attributes)
}

func sameRawPrefix(left, right [][]byte) bool {
	count := len(left)
	if len(right) < count {
		count = len(right)
	}
	for index := 0; index < count; index++ {
		if !bytes.Equal(left[index], right[index]) {
			return false
		}
	}
	return true
}

func prefixLines(source *rollout, limit uint64) ([][]byte, error) {
	var size uint64
	for index, line := range source.RawLines {
		size += uint64(len(line)) + 1
		if size == limit {
			return source.RawLines[:index+1], nil
		}
		if size > limit {
			break
		}
	}
	return nil, fmt.Errorf("history byte offset is outside a complete source prefix for rollout %s", source.RolloutID)
}

func validateReferencedReplacement(root string, source *rollout, paths map[string]string, headers map[string]rolloutHeader, retiring map[string]string) error {
	existing, ok := paths[source.RolloutID]
	if !ok {
		return nil
	}
	for _, header := range headers {
		base := header.HistoryBase
		if _, removed := retiring[header.RolloutID]; removed {
			continue
		}
		if header.RolloutID == source.RolloutID || base == nil || base.ThreadID != source.RolloutID {
			continue
		}
		desired, err := prefixLines(source, base.EndByteOffset)
		if err != nil {
			return fmt.Errorf("cannot replace referenced rollout %s: %w", source.RolloutID, err)
		}
		current, err := readRolloutPrefix(existing, root, nil, base.EndByteOffset)
		if err != nil {
			return err
		}
		if source.HistoryMode != "paginated" || checksumLines(desired) != current.RawChecksum ||
			source.EndOrdinal-uint64(len(source.RawLines))+uint64(len(desired)) != base.EndOrdinalExclusive {
			return fmt.Errorf("cannot replace referenced rollout %s: another session requires its existing raw prefix", source.RolloutID)
		}
	}
	return nil
}

func materializeDependency(home string, dependency *rollout, existing map[string]string) error {
	target := filepath.Join(home, filepath.FromSlash(dependency.Relative))
	compressed := dependency.Compressed
	if path, exists := existing[dependency.RolloutID]; exists {
		current, err := readRolloutBounded(path, "", nil, rolloutBytes(dependency), true)
		if err != nil {
			return err
		}
		if !sameRawPrefix(current.RawLines, dependency.RawLines) {
			return fmt.Errorf("history source prefix conflict for rollout %s", dependency.RolloutID)
		}
		if len(current.RawLines) >= len(dependency.RawLines) {
			return nil
		}
		target = path
		compressed = strings.HasSuffix(path, ".zst")
	}
	if err := writeRollout(target, dependency.RawLines, compressed); err != nil {
		return err
	}
	existing[dependency.RolloutID] = target
	return nil
}

// Merge dependency requirements across the entire reconciliation before making
// changes. Shared sources are materialized once at the longest compatible
// boundary, irrespective of logical session ordering.
func planHistoryImports(root, home string, projects projectState, profiles map[string]*rollout, reg *registry) (map[string]*rollout, map[string]string, error) {
	required := map[string]*rollout{}
	tips := map[string]*rollout{}
	for id, source := range projects.Sessions {
		previous := profiles[id]
		base, owned := reg.Sessions[id]
		importTip := previous == nil && !owned
		if previous != nil {
			conflict, winner := compareSessions(source, previous, base, owned)
			importTip = conflict == "" && winner == "project"
		}
		if !importTip {
			continue
		}
		if _, err := preflightHistoryImport(root, home, source); err != nil {
			return nil, nil, err
		}
		tips[source.RolloutID] = source
		for _, dependency := range source.Dependencies {
			current := required[dependency.RolloutID]
			if current != nil && !sameRawPrefix(current.RawLines, dependency.RawLines) {
				return nil, nil, fmt.Errorf("incompatible planned history prefixes for rollout %s", dependency.RolloutID)
			}
			if current == nil || len(dependency.RawLines) > len(current.RawLines) {
				required[dependency.RolloutID] = dependency
			}
		}
	}
	for physical, source := range tips {
		if dependency := required[physical]; dependency != nil {
			if !sameRawPrefix(source.RawLines, dependency.RawLines) {
				return nil, nil, fmt.Errorf("planned source conflicts with dependency rollout %s", physical)
			}
			if len(source.RawLines) < len(dependency.RawLines) {
				return nil, nil, fmt.Errorf("selected source rollout %s is shorter than a required history prefix", physical)
			}
			if len(source.RawLines) > len(dependency.RawLines) {
				required[physical] = source
			}
		}
	}
	paths, _, err := profileCatalog(home)
	return required, paths, err
}

func profileTombstonePaths(home string, projects projectState, logical string) ([]string, error) {
	paths, headers, err := profileCatalog(home)
	if err != nil {
		return nil, err
	}
	protected := map[string]bool{}
	var protect func(string)
	protect = func(physical string) {
		if protected[physical] {
			return
		}
		protected[physical] = true
		if base := headers[physical].HistoryBase; base != nil {
			protect(base.ThreadID)
		}
	}
	for _, header := range headers {
		if header.ID != logical && header.HistoryBase != nil {
			protect(header.HistoryBase.ThreadID)
		}
	}
	for _, source := range projects.Sessions {
		for _, dependency := range source.Dependencies {
			protect(dependency.RolloutID)
		}
	}
	var remove []string
	for physical, header := range headers {
		if header.ID == logical && !protected[physical] {
			remove = append(remove, paths[physical])
		}
	}
	sort.Strings(remove)
	return remove, nil
}

func hideDeletedProfileTips(home string, projects projectState, profiles map[string]*rollout, reg *registry) error {
	paths, _, err := profileCatalog(home)
	if err != nil {
		return err
	}
	for id, base := range reg.Sessions {
		if base.Tombstone || base.RolloutID == "" {
			continue
		}
		if _, exists := paths[base.RolloutID]; exists {
			continue
		}
		project, profile := projects.Sessions[id], profiles[id]
		if project == nil || profile == nil {
			continue
		}
		for _, dependency := range project.Dependencies {
			if dependency.RolloutID == profile.RolloutID {
				delete(profiles, id)
				break
			}
		}
	}
	return nil
}

func validChecksum(value string) bool {
	if len(value) != 64 {
		return false
	}
	for _, char := range value {
		if !(char >= '0' && char <= '9') && !(char >= 'a' && char <= 'f') {
			return false
		}
	}
	return true
}

func validateProjectPhysicalSnapshots(state projectState) error {
	physical := map[string]*rollout{}
	selected := map[string]*rollout{}
	for _, source := range state.Sessions {
		if _, duplicate := selected[source.RolloutID]; duplicate {
			return fmt.Errorf("duplicate selected physical rollout %s", source.RolloutID)
		}
		selected[source.RolloutID] = source
		for _, item := range append(append([]*rollout{}, source.Dependencies...), source) {
			previous := physical[item.RolloutID]
			if previous != nil && !sameRawPrefix(previous.RawLines, item.RawLines) {
				return fmt.Errorf("incompatible history snapshots for physical rollout %s", item.RolloutID)
			}
			if previous == nil || len(item.RawLines) > len(previous.RawLines) {
				physical[item.RolloutID] = item
			}
		}
	}
	groups := map[string]map[string]bool{}
	referenced := map[string]bool{}
	for id, source := range physical {
		if groups[source.ID] == nil {
			groups[source.ID] = map[string]bool{}
		}
		groups[source.ID][id] = true
		if base := source.HistoryBase; base != nil {
			if ancestor := physical[base.ThreadID]; ancestor != nil && ancestor.ID == source.ID {
				referenced[base.ThreadID] = true
			}
		}
	}
	for logical, members := range groups {
		var tip string
		for physical := range members {
			if !referenced[physical] {
				if tip != "" {
					return fmt.Errorf("ambiguous inherited rollout branches for session %s", logical)
				}
				tip = physical
			}
		}
		if tip == "" {
			return fmt.Errorf("cyclic inherited rollout graph for session %s", logical)
		}
		if current := state.Sessions[logical]; current != nil && current.RolloutID != tip {
			return fmt.Errorf("selected rollout is not the unique inherited history tip for session %s", logical)
		}
	}
	for id, source := range selected {
		if len(source.RawLines) < len(physical[id].RawLines) {
			return fmt.Errorf("selected source rollout %s is shorter than a required history prefix", id)
		}
	}
	return nil
}

// A chosen physical tip may replace a divergent old branch only if that branch
// can be retired completely. Keeping a referenced sibling would leave native
// current-tip selection ambiguous, so refuse before changing either copy.
func profileRetirementPlan(home string, source *rollout, paths map[string]string, headers map[string]rolloutHeader) (map[string]string, error) {
	retained := map[string]bool{source.RolloutID: true}
	for _, dependency := range source.Dependencies {
		retained[dependency.RolloutID] = true
	}
	retiring := map[string]string{}
	for physical, header := range headers {
		if header.ID == source.ID && !retained[physical] {
			if header.ValidationError != nil {
				return nil, header.ValidationError
			}
			retiring[physical] = paths[physical]
		}
	}
	for _, header := range headers {
		if _, removed := retiring[header.RolloutID]; removed {
			continue
		}
		if base := header.HistoryBase; base != nil {
			if _, removed := retiring[base.ThreadID]; removed {
				return nil, fmt.Errorf("cannot replace rollout branch for session %s: another session still references rollout %s", source.ID, base.ThreadID)
			}
		}
	}
	return retiring, nil
}
