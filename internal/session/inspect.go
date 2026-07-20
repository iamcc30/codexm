package session

import (
	"errors"
	"os"
)

// Inspection is a read-only view of a project's portable session mirror.
// Inspect never creates locks, registries, directories, or metadata.
type Inspection struct {
	Enabled          bool   `json:"enabled"`
	ProjectRoot      string `json:"project_root,omitempty"`
	ProjectID        string `json:"project_id,omitempty"`
	Sessions         int    `json:"sessions"`
	ArchivedSessions int    `json:"archived_sessions"`
	Tombstones       int    `json:"tombstones"`
	Pending          Result `json:"pending"`
	ValidationError  string `json:"validation_error,omitempty"`
}

func Inspect(projectRoot, profileHome, managerHome string) Inspection {
	root, project, err := FindProject(projectRoot)
	if err != nil {
		if errors.Is(err, ErrNotInitialized) || errors.Is(err, os.ErrNotExist) {
			return Inspection{}
		}
		return Inspection{ValidationError: err.Error()}
	}
	out := Inspection{Enabled: true, ProjectRoot: root, ProjectID: project.ProjectID}
	projectState, err := scanProject(root)
	if err != nil {
		out.ValidationError = err.Error()
		return out
	}
	for _, item := range projectState.Sessions {
		if item.Archived {
			out.ArchivedSessions++
		} else {
			out.Sessions++
		}
	}
	out.Tombstones = len(projectState.Tombstones)
	if profileHome == "" || managerHome == "" {
		return out
	}
	opts := Options{ProjectRoot: root, ProfileHome: profileHome, ManagerHome: managerHome}
	root, project, err = resolveOptions(&opts)
	if err != nil {
		out.ValidationError = err.Error()
		return out
	}
	out.Pending, err = reconcile(opts, root, project, false)
	if err != nil {
		out.ValidationError = err.Error()
	}
	return out
}
