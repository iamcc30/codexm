package session

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"
)

type ResolveOptions struct {
	ProjectRoot string
	ProfileHome string
	ManagerHome string
	SessionID   string
	Use         string
}

type ResolveResult struct {
	SessionID string
	Winner    string
	Backup    string
}

func Resolve(opts ResolveOptions) (ResolveResult, error) {
	if !validUUID(opts.SessionID) {
		return ResolveResult{}, fmt.Errorf("invalid session id %q", opts.SessionID)
	}
	if opts.Use != "project" && opts.Use != "profile" {
		return ResolveResult{}, errors.New("--use must be project or profile")
	}
	syncOpts := Options{
		ProjectRoot: opts.ProjectRoot,
		ProfileHome: opts.ProfileHome,
		ManagerHome: opts.ManagerHome,
	}
	root, project, err := resolveOptions(&syncOpts)
	if err != nil {
		return ResolveResult{}, err
	}
	unlock, err := acquireLock(opts.ManagerHome, project.ProjectID)
	if err != nil {
		return ResolveResult{}, err
	}
	defer unlock()

	projectState, err := scanProject(root)
	if err != nil {
		return ResolveResult{}, err
	}
	profileState, err := scanProfile(syncOpts.ProfileHome, root)
	if err != nil {
		return ResolveResult{}, err
	}
	projectSession := projectState.Sessions[opts.SessionID]
	profileSession := profileState[opts.SessionID]
	if projectSession == nil || profileSession == nil {
		return ResolveResult{}, fmt.Errorf("session %s does not have both project and profile copies", opts.SessionID)
	}
	reg, registryPath, err := loadRegistry(opts.ManagerHome, project, root, syncOpts.ProfileHome)
	if err != nil {
		return ResolveResult{}, err
	}
	base, owned := reg.Sessions[opts.SessionID]
	conflict, _ := compareSessions(projectSession, profileSession, base, owned)
	if conflict == "" {
		return ResolveResult{}, fmt.Errorf("session %s is not currently conflicted", opts.SessionID)
	}

	if opts.Use == "profile" {
		projected := projectState
		projected.Sessions = make(map[string]*rollout, len(projectState.Sessions))
		for id, item := range projectState.Sessions {
			projected.Sessions[id] = item
		}
		projected.Sessions[opts.SessionID] = profileSession
		if err := validateProjectPhysicalSnapshots(projected); err != nil {
			return ResolveResult{}, err
		}
	}
	loser := profileSession
	if opts.Use == "profile" {
		loser = projectSession
	}
	var retiring []string
	if opts.Use == "project" {
		if _, err := preflightHistoryImport(root, syncOpts.ProfileHome, projectSession); err != nil {
			return ResolveResult{}, err
		}
		paths, headers, err := profileCatalog(syncOpts.ProfileHome)
		if err != nil {
			return ResolveResult{}, err
		}
		plan, err := profileRetirementPlan(syncOpts.ProfileHome, projectSession, paths, headers)
		if err != nil {
			return ResolveResult{}, err
		}
		for _, physical := range sortedIDs(plan) {
			retiring = append(retiring, plan[physical])
		}
	}
	backup, err := backupSession(opts.ManagerHome, project.ProjectID, opts.Use, loser, retiring...)
	if err != nil {
		return ResolveResult{}, err
	}
	if opts.Use == "project" {
		if err := importProjectSession(root, syncOpts.ProfileHome, projectSession, profileSession); err != nil {
			return ResolveResult{}, err
		}
		reg.Sessions[opts.SessionID] = registrationFor(projectSession)
	} else {
		if err := exportProfileSession(root, profileSession, projectSession); err != nil {
			return ResolveResult{}, err
		}
		reg.Sessions[opts.SessionID] = registrationFor(profileSession)
	}
	if err := atomicWriteJSON(registryPath, reg, 0o600); err != nil {
		return ResolveResult{}, err
	}
	return ResolveResult{SessionID: opts.SessionID, Winner: opts.Use, Backup: backup}, nil
}

func backupSession(managerHome, projectID, winner string, source *rollout, retiring ...string) (string, error) {
	stamp := time.Now().UTC().Format("20060102T150405.000000000Z")
	loser := "profile"
	if winner == "profile" {
		loser = "project"
	}
	extension := ".jsonl"
	if source.Compressed {
		extension += ".zst"
	}
	dir := filepath.Join(managerHome, "session-backups", projectID, stamp+"-"+source.ID)
	originals := append([]string{source.Path}, retiring...)
	for _, dependency := range source.Dependencies {
		originals = append(originals, dependency.Path)
	}
	if err := backupOriginalRollouts(dir, originals); err != nil {
		return "", err
	}
	path := filepath.Join(dir, loser+extension)
	if usesHistory(source) {
		path = filepath.Join(dir, filepath.FromSlash(source.Relative))
		for _, dependency := range source.Dependencies {
			descriptor := dependencyFor(dependency)
			if err := writeRollout(filepath.Join(dir, filepath.FromSlash(descriptor.SnapshotPath)), dependency.RawLines, dependency.Compressed); err != nil {
				return "", err
			}
		}
	}
	if err := writeRollout(path, source.RawLines, source.Compressed); err != nil {
		return "", err
	}
	metadata := metadataFor(source)
	if err := atomicWriteJSON(filepath.Join(dir, loser+".metadata.json"), metadata, 0o600); err != nil {
		return "", err
	}
	return strings.TrimSpace(path), nil
}

type originalRolloutBackup struct {
	RolloutID    string `json:"rollout_id"`
	OriginalPath string `json:"original_path"`
	BackupPath   string `json:"backup_path"`
	FileChecksum string `json:"file_checksum"`
}

// Preserve full on-disk originals as well as the logical bounded history. A
// losing ancestor can have a legitimate tail beyond its child's inherited
// cutoff, and retiring that ancestor must never discard the tail unbacked.
func backupOriginalRollouts(dir string, paths []string) error {
	seen := map[string]bool{}
	var manifest []originalRolloutBackup
	for _, path := range paths {
		if seen[path] {
			continue
		}
		seen[path] = true
		header, err := readRolloutHeader(path)
		if err != nil {
			return err
		}
		if header.ValidationError != nil {
			return header.ValidationError
		}
		data, err := os.ReadFile(path)
		if err != nil {
			return err
		}
		relative := filepath.ToSlash(filepath.Join("originals", header.RolloutID, filepath.Base(path)))
		if err := atomicWrite(filepath.Join(dir, filepath.FromSlash(relative)), data, 0o600); err != nil {
			return err
		}
		manifest = append(manifest, originalRolloutBackup{RolloutID: header.RolloutID, OriginalPath: path, BackupPath: relative, FileChecksum: checksumBytes(data)})
	}
	return atomicWriteJSON(filepath.Join(dir, "originals.json"), manifest, 0o600)
}
