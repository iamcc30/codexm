package session

import (
	"errors"
	"fmt"
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

	loser := profileSession
	if opts.Use == "profile" {
		loser = projectSession
	}
	backup, err := backupSession(opts.ManagerHome, project.ProjectID, opts.Use, loser)
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

func backupSession(managerHome, projectID, winner string, source *rollout) (string, error) {
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
	path := filepath.Join(dir, loser+extension)
	if err := writeRollout(path, source.RawLines, source.Compressed); err != nil {
		return "", err
	}
	metadata := Metadata{
		FormatVersion: FormatVersion,
		SessionID:     source.ID,
		RolloutPath:   source.Relative,
		Checksum:      source.Checksum,
		LineCount:     source.LineCount,
		Archived:      source.Archived,
		Name:          source.Name,
		CWDs:          source.CWDs,
	}
	if err := atomicWriteJSON(filepath.Join(dir, loser+".metadata.json"), metadata, 0o600); err != nil {
		return "", err
	}
	return strings.TrimSpace(path), nil
}
