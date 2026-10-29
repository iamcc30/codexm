package session

import (
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"
)

type Options struct {
	Lease          *Lease
	ProjectRoot    string
	ProfileHome    string
	ManagerHome    string
	ImportExisting bool
	DryRun         bool
	Eligible       map[string]bool
	OnlyEligible   bool
}

type Result struct {
	Imported       int
	Exported       int
	UpdatedProject int
	UpdatedProfile int
	DeletedProject int
	DeletedProfile int
	Ignored        int
	Conflicts      []Conflict
	Actions        []string
	DryRun         bool
}

type Conflict struct {
	SessionID string
	Reason    string
}

type ConflictError struct {
	Conflicts []Conflict
}

func (e *ConflictError) Error() string {
	ids := make([]string, 0, len(e.Conflicts))
	for _, conflict := range e.Conflicts {
		ids = append(ids, conflict.SessionID)
	}
	return fmt.Sprintf("session conflict detected for %s; resolve with `codexm session resolve --use project|profile SESSION_ID`", strings.Join(ids, ", "))
}

type registration struct {
	RolloutID string `json:"rollout_id,omitempty"`
	Checksum  string `json:"checksum,omitempty"`
	Archived  bool   `json:"archived,omitempty"`
	Name      string `json:"name,omitempty"`
	Tombstone bool   `json:"tombstone,omitempty"`
}

type registry struct {
	FormatVersion int                     `json:"format_version"`
	ProjectID     string                  `json:"project_id"`
	ProjectRoot   string                  `json:"project_root"`
	ProfileHome   string                  `json:"profile_home"`
	Sessions      map[string]registration `json:"sessions"`
}

func Sync(opts Options) (Result, error) {
	root, project, err := resolveOptions(&opts)
	if err != nil {
		return Result{}, err
	}
	releaseLease, err := acquireOperationLease(root, opts.ProfileHome, opts.Lease)
	if err != nil {
		return Result{}, err
	}
	defer releaseLease()
	unlock, err := acquireLock(opts.ManagerHome, project.ProjectID)
	if err != nil {
		return Result{}, err
	}
	defer unlock()
	if !opts.DryRun {
		if err := ensureLocalIgnores(root); err != nil {
			return Result{}, err
		}
		if err := recoverProjectTransaction(root); err != nil {
			return Result{}, err
		}
		if err := recoverProfileTransaction(root, opts.ProfileHome); err != nil {
			return Result{}, err
		}
	}

	projectSessions, profileSessions, reg, registryPath, err := loadReconcileState(opts, root, project)
	if err != nil {
		return Result{}, err
	}
	result, err := reconcileState(opts, root, project, projectSessions, profileSessions, cloneRegistry(reg), registryPath, false)
	if err != nil {
		return result, err
	}
	if opts.DryRun {
		result.DryRun = true
	}
	if len(result.Conflicts) > 0 {
		return result, &ConflictError{Conflicts: result.Conflicts}
	}
	if opts.DryRun {
		return result, nil
	}
	return reconcileState(opts, root, project, projectSessions, profileSessions, cloneRegistry(reg), registryPath, true)
}

func SnapshotProfile(profileHome, projectRoot string) (map[string]bool, error) {
	return scanProfileIDs(profileHome, projectRoot)
}

func NewSessions(before, after map[string]bool) map[string]bool {
	result := map[string]bool{}
	for id := range after {
		if !before[id] {
			result[id] = true
		}
	}
	return result
}

func reconcile(opts Options, root string, project Project, apply bool) (Result, error) {
	projectSessions, profileSessions, reg, registryPath, err := loadReconcileState(opts, root, project)
	if err != nil {
		return Result{}, err
	}
	return reconcileState(opts, root, project, projectSessions, profileSessions, reg, registryPath, apply)
}

func loadReconcileState(opts Options, root string, project Project) (projectState, map[string]*rollout, *registry, string, error) {
	projectSessions, err := scanProject(root)
	if err != nil {
		return projectState{}, nil, nil, "", err
	}
	profileSessions, err := scanProfile(opts.ProfileHome, root)
	if err != nil {
		return projectState{}, nil, nil, "", err
	}
	reg, registryPath, err := loadRegistry(opts.ManagerHome, project, root, opts.ProfileHome)
	if err != nil {
		return projectState{}, nil, nil, "", err
	}
	if err := hideDeletedProfileTips(opts.ProfileHome, projectSessions, profileSessions, reg); err != nil {
		return projectState{}, nil, nil, "", err
	}
	return projectSessions, profileSessions, reg, registryPath, nil
}

func cloneRegistry(source *registry) *registry {
	cloned := &registry{
		FormatVersion: source.FormatVersion,
		ProjectID:     source.ProjectID,
		ProjectRoot:   source.ProjectRoot,
		ProfileHome:   source.ProfileHome,
		Sessions:      make(map[string]registration, len(source.Sessions)),
	}
	for id, item := range source.Sessions {
		cloned.Sessions[id] = item
	}
	return cloned
}

func reconcileState(opts Options, root string, project Project, projectSessions projectState, profileSessions map[string]*rollout, reg *registry, registryPath string, apply bool) (Result, error) {
	result := Result{}
	required, _, err := planHistoryImports(root, opts.ProfileHome, projectSessions, profileSessions, reg)
	if err != nil {
		return result, err
	}
	profileOperations, err := planProfileImportTransaction(root, opts.ProfileHome, selectedProfileImports(projectSessions, profileSessions, reg), required)
	if err != nil {
		return result, err
	}
	if apply && len(profileOperations) > 0 {
		if err := applyProfileTransaction(root, opts.ProfileHome, profileOperations); err != nil {
			return result, err
		}
	}

	ids := make(map[string]bool)
	for id := range projectSessions.Sessions {
		ids[id] = true
	}
	for id := range profileSessions {
		ids[id] = true
	}
	for id := range projectSessions.Tombstones {
		ids[id] = true
	}
	for id := range reg.Sessions {
		ids[id] = true
	}
	ordered := make([]string, 0, len(ids))
	for id := range ids {
		ordered = append(ordered, id)
	}
	sort.Strings(ordered)

	for _, id := range ordered {
		projectSession := projectSessions.Sessions[id]
		profileSession := profileSessions[id]
		tombstone, tombstoned := projectSessions.Tombstones[id]
		base, owned := reg.Sessions[id]

		if tombstoned {
			if projectSession != nil {
				return result, fmt.Errorf("session %s has both rollout and tombstone", id)
			}
			if owned {
				if profileSession != nil {
					paths, err := profileTombstonePaths(opts.ProfileHome, projectSessions, id)
					if err != nil {
						return result, err
					}
					if len(paths) > 0 {
						result.DeletedProfile++
						result.Actions = append(result.Actions, "delete profile session "+id)
						if apply {
							var operations []projectMutation
							for _, path := range paths {
								relative, err := filepath.Rel(opts.ProfileHome, path)
								if err != nil {
									return result, err
								}
								operations = append(operations, projectMutation{Path: filepath.ToSlash(relative), Delete: true, Mode: 0o600})
							}
							if err := applyProfileTransaction(root, opts.ProfileHome, operations); err != nil {
								return result, err
							}
						}
					}
				}

				reg.Sessions[id] = registration{Checksum: tombstone.Checksum, Tombstone: true}
			} else if profileSession != nil {
				result.Ignored++
			}
			continue
		}
		if owned && base.Tombstone && (projectSession != nil || profileSession != nil) {
			return result, fmt.Errorf("tombstoned session %s was reintroduced without its tombstone", id)
		}

		switch {
		case projectSession != nil && profileSession != nil:
			conflict, winner := compareSessions(projectSession, profileSession, base, owned)
			if conflict != "" {
				result.Conflicts = append(result.Conflicts, Conflict{SessionID: id, Reason: conflict})
				continue
			}
			switch winner {
			case "project":
				if _, err := preflightHistoryImport(root, opts.ProfileHome, projectSession); err != nil {
					return result, err
				}
				changed := projectSession.Checksum != profileSession.Checksum ||
					projectSession.Archived != profileSession.Archived ||
					projectSession.Name != profileSession.Name || (usesHistory(projectSession) && projectSession.ProjectMeta.FormatVersion == FormatVersion)
				if changed {
					result.UpdatedProfile++
					result.Actions = append(result.Actions, "update profile session "+id)
					if apply {
						if err := importProjectSession(root, opts.ProfileHome, projectSession, profileSession); err != nil {
							return result, err
						}
					}
				}
				reg.Sessions[id] = registrationFor(projectSession)
			case "profile":
				changed := projectSession.Checksum != profileSession.Checksum ||
					projectSession.Archived != profileSession.Archived ||
					projectSession.Name != profileSession.Name || (usesHistory(projectSession) && projectSession.ProjectMeta.FormatVersion == FormatVersion)
				if changed {
					result.UpdatedProject++
					result.Actions = append(result.Actions, "update project session "+id)
					if apply {
						if err := exportProfileSession(root, profileSession, projectSession); err != nil {
							return result, err
						}
					}
				}
				reg.Sessions[id] = registrationFor(profileSession)
			default:
				reg.Sessions[id] = registrationFor(projectSession)
			}

		case projectSession != nil:
			if owned && !base.Tombstone {
				result.DeletedProject++
				result.Actions = append(result.Actions, "tombstone deleted profile session "+id)
				if apply {
					if err := tombstoneProjectSession(root, projectSession); err != nil {
						return result, err
					}
				}
				reg.Sessions[id] = registration{Checksum: projectSession.Checksum, Tombstone: true}
				continue
			}
			if _, err := preflightHistoryImport(root, opts.ProfileHome, projectSession); err != nil {
				return result, err
			}
			result.Imported++
			result.Actions = append(result.Actions, "import project session "+id)
			if apply {
				if err := importProjectSession(root, opts.ProfileHome, projectSession, nil); err != nil {
					return result, err
				}
			}
			reg.Sessions[id] = registrationFor(projectSession)

		case profileSession != nil:
			if owned && !base.Tombstone {
				return result, fmt.Errorf("managed project session %s was removed without a tombstone", id)
			}
			if eligibleProfileSession(opts, project, root, profileSession) {
				result.Exported++
				result.Actions = append(result.Actions, "export profile session "+id)
				if apply {
					if err := exportProfileSession(root, profileSession, nil); err != nil {
						return result, err
					}
				}
				reg.Sessions[id] = registrationFor(profileSession)
			} else {
				result.Ignored++
			}

		default:
			if owned && !base.Tombstone {
				return result, fmt.Errorf("managed session %s is missing from both project and profile without a tombstone", id)
			}
		}
	}

	if len(result.Conflicts) > 0 || !apply {
		return result, nil
	}
	if err := atomicWriteJSON(registryPath, reg, 0o600); err != nil {
		return result, err
	}
	return result, nil
}

func compareSessions(project, profile *rollout, base registration, owned bool) (string, string) {
	if historyPrefixConflict(project, profile) {
		return "inherited raw history prefixes differ", ""
	}
	for _, dependency := range project.Dependencies {
		if dependency.RolloutID == profile.RolloutID {
			if owned && (profile.Checksum != base.Checksum || profile.Name != base.Name || profile.Archived != base.Archived) {
				return "profile changed independently of the project history transition", ""
			}
			return "", "project"
		}
	}
	for _, dependency := range profile.Dependencies {
		if dependency.RolloutID == project.RolloutID {
			if owned && (project.Checksum != base.Checksum || project.Name != base.Name || project.Archived != base.Archived) {
				return "project changed independently of the profile history transition", ""
			}
			return "", "profile"
		}
	}
	relation := compareCanonical(project.Canonical, profile.Canonical)
	projectChanged := !owned || project.Checksum != base.Checksum ||
		project.Archived != base.Archived || project.Name != base.Name
	profileChanged := !owned || profile.Checksum != base.Checksum ||
		profile.Archived != base.Archived || profile.Name != base.Name

	switch relation {
	case "diverged":
		return "both copies contain independent JSONL appends", ""
	case "project-longer":
		if owned && profileChanged && projectChanged {
			return "project content advanced while profile metadata also changed", ""
		}
		return "", "project"
	case "profile-longer":
		if owned && projectChanged && profileChanged {
			return "profile content advanced while project metadata also changed", ""
		}
		return "", "profile"
	}

	if project.Archived == profile.Archived && project.Name == profile.Name {
		if usesHistory(project) && project.ProjectMeta.FormatVersion == FormatVersion {
			return "", "profile"
		}
		return "", "equal"
	}
	if !owned {
		return "", "project"
	}
	switch {
	case projectChanged && profileChanged:
		return "both copies changed archive state or session name", ""
	case projectChanged:
		return "", "project"
	case profileChanged:
		return "", "profile"
	default:
		return "copies disagree with the last synchronized metadata", ""
	}
}

func compareCanonical(project, profile [][]byte) string {
	common := len(project)
	if len(profile) < common {
		common = len(profile)
	}
	for i := 0; i < common; i++ {
		if string(project[i]) != string(profile[i]) {
			return "diverged"
		}
	}
	switch {
	case len(project) > len(profile):
		return "project-longer"
	case len(profile) > len(project):
		return "profile-longer"
	default:
		return "equal"
	}
}

func eligibleProfileSession(opts Options, project Project, root string, session *rollout) bool {
	if _, within := relativeWithin(root, session.InitialCWD); !within {
		return false
	}
	if opts.ImportExisting || opts.Eligible[session.ID] {
		return true
	}
	if opts.OnlyEligible {
		return false
	}
	created, err := time.Parse(time.RFC3339Nano, session.Timestamp)
	if err != nil {
		return false
	}
	initialized, err := time.Parse(time.RFC3339Nano, project.InitializedAt)
	return err == nil && !created.Before(initialized)
}

func exportProfileSession(root string, source, previous *rollout) error {
	if usesHistory(source) {
		if err := requireHistoryVersion(root); err != nil {
			return err
		}
	}
	if err := writeHistoryDependencies(root, source); err != nil {
		return err
	}
	dotDir := filepath.Join(root, projectDirName)
	relative := filepath.Clean(filepath.FromSlash(source.Relative))
	if !safeRelative(relative) {
		return fmt.Errorf("unsafe profile session path %q", source.Relative)
	}
	target := filepath.Join(dotDir, relative)
	data, err := encodeRollout(source.RawLines, source.Compressed)
	if err != nil {
		return err
	}
	metadata, err := projectJSONMutation("metadata/"+source.ID+".json", metadataFor(source))
	if err != nil {
		return err
	}
	operations := []projectMutation{{Path: filepath.ToSlash(relative), Data: data, Mode: 0o600}, metadata}
	if previous != nil && previous.Path != target {
		old, err := filepath.Rel(dotDir, previous.Path)
		if err != nil {
			return err
		}
		operations = append(operations, projectMutation{Path: filepath.ToSlash(old), Delete: true, Mode: 0o600})
	}
	return applyProjectTransaction(root, operations)
}

func importProjectSession(root, profileHome string, source, previous *rollout) error {
	_, err := preflightHistoryImport(root, profileHome, source)
	if err != nil {
		return err
	}
	if usesHistory(source) && source.ProjectMeta.FormatVersion == FormatVersion {
		if err := requireHistoryVersion(root); err != nil {
			return err
		}
		metadata, err := projectJSONMutation("metadata/"+source.ID+".json", metadataFor(source))
		if err != nil {
			return err
		}
		if err := applyProjectTransaction(root, []projectMutation{metadata}); err != nil {
			return err
		}
	}
	required := map[string]*rollout{}
	for _, dependency := range source.Dependencies {
		required[dependency.RolloutID] = dependency
	}
	operations, err := planProfileImportTransaction(root, profileHome, []*rollout{source}, required)
	if err != nil {
		return err
	}
	if len(operations) > 0 {
		if err := applyProfileTransaction(root, profileHome, operations); err != nil {
			return err
		}
	}

	return nil
}

func tombstoneProjectSession(root string, source *rollout) error {
	relative, err := filepath.Rel(filepath.Join(root, projectDirName), source.Path)
	if err != nil {
		return err
	}
	tombstone := Tombstone{FormatVersion: FormatVersion, SessionID: source.ID, DeletedAt: nowRFC3339(), Checksum: source.Checksum}
	mutation, err := projectJSONMutation("tombstones/"+source.ID+".json", tombstone)
	if err != nil {
		return err
	}
	return applyProjectTransaction(root, []projectMutation{mutation,
		{Path: "metadata/" + source.ID + ".json", Delete: true, Mode: 0o644},
		{Path: filepath.ToSlash(relative), Delete: true, Mode: 0o600},
	})
}

func removeRollout(path string) error {
	if err := rejectSymlink(path); err != nil {
		return err
	}
	if err := os.Remove(path); err != nil && !errors.Is(err, os.ErrNotExist) {
		return fmt.Errorf("remove %s: %w", path, err)
	}
	return nil
}

func registrationFor(session *rollout) registration {
	return registration{
		RolloutID: session.RolloutID,
		Checksum:  session.Checksum,
		Archived:  session.Archived,
		Name:      session.Name,
	}
}

func resolveOptions(opts *Options) (string, Project, error) {
	if opts.ProfileHome == "" {
		return "", Project{}, errors.New("profile home is required")
	}
	if opts.ManagerHome == "" {
		return "", Project{}, errors.New("manager home is required")
	}
	root, project, err := FindProject(opts.ProjectRoot)
	if err != nil {
		return "", Project{}, err
	}
	profileHome, err := filepath.Abs(opts.ProfileHome)
	if err != nil {
		return "", Project{}, err
	}
	opts.ProfileHome = filepath.Clean(profileHome)
	return root, project, nil
}

func loadRegistry(managerHome string, project Project, root, profileHome string) (*registry, string, error) {
	profileKey := pathKey(profileHome)
	path := filepath.Join(managerHome, "session-projects", project.ProjectID, profileKey+".json")
	reg := &registry{
		FormatVersion: FormatVersion,
		ProjectID:     project.ProjectID,
		ProjectRoot:   root,
		ProfileHome:   profileHome,
		Sessions:      map[string]registration{},
	}
	data, err := os.ReadFile(path)
	if errors.Is(err, os.ErrNotExist) {
		return reg, path, nil
	}
	if err != nil {
		return nil, "", err
	}
	if err := json.Unmarshal(data, reg); err != nil {
		return nil, "", fmt.Errorf("invalid local session registry %s: %w", path, err)
	}
	if reg.FormatVersion != FormatVersion || reg.ProjectID != project.ProjectID ||
		filepath.Clean(reg.ProfileHome) != profileHome {
		return nil, "", fmt.Errorf("local session registry does not match project/profile: %s", path)
	}
	if reg.Sessions == nil {
		reg.Sessions = map[string]registration{}
	}
	reg.ProjectRoot = root
	return reg, path, nil
}

func nowRFC3339() string {
	return time.Now().UTC().Format(time.RFC3339Nano)
}
