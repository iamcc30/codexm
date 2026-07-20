package cli

import (
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"os"
	"strings"

	"github.com/iamcc30/codexm/internal/config"
	"github.com/iamcc30/codexm/internal/session"
)

func (a *App) cmdSession(args []string) int {
	if len(args) == 0 || args[0] == "help" || args[0] == "--help" || args[0] == "-h" {
		a.printSessionHelp()
		return 0
	}
	switch args[0] {
	case "init":
		return a.cmdSessionInit(args[1:])
	case "sync":
		return a.cmdSessionSync(args[1:], false)
	case "status":
		return a.cmdSessionSync(args[1:], true)
	case "resolve":
		return a.cmdSessionResolve(args[1:])
	case "audit":
		return a.cmdSessionAudit(args[1:])
	default:
		fmt.Fprintf(a.Err, "unknown session command %q\n\n", args[0])
		a.printSessionHelp()
		return 2
	}
}

func (a *App) printSessionHelp() {
	fmt.Fprint(a.Out, `Manage portable Codex session mirrors for a project.

Usage:
  codexm session init [--project PATH] [--profile PROFILE] [--import-existing]
  codexm session sync [--project PATH] [--profile PROFILE]
  codexm session status [--project PATH] [--profile PROFILE]
  codexm session resolve [--project PATH] [--profile PROFILE] --use project|profile SESSION_ID
  codexm session audit [--project PATH] [--strict] [--json] [--max-file-size-mb N]

Project session management is opt-in. Session JSONL may contain prompts, command
output, absolute paths, and secrets. Prefer a private repository and review
.codexm/ before committing it.
`)
}

func (a *App) cmdSessionAudit(args []string) int {
	fs := flag.NewFlagSet("session audit", flag.ContinueOnError)
	fs.SetOutput(a.Err)
	projectPath := fs.String("project", "", "project path (nearest initialized parent is used)")
	strict := fs.Bool("strict", false, "fail when warnings are found in addition to errors")
	jsonOutput := fs.Bool("json", false, "write machine-readable JSON")
	maxMiB := fs.Int64("max-file-size-mb", session.DefaultAuditMaxFileSize/(1024*1024), "warn when a transcript exceeds this decompressed size")
	if err := fs.Parse(args); err != nil {
		return 2
	}
	if fs.NArg() != 0 || *maxMiB <= 0 {
		fmt.Fprintln(a.Err, "usage: codexm session audit [--project PATH] [--strict] [--json] [--max-file-size-mb N]")
		return 2
	}
	path := *projectPath
	if path == "" {
		var err error
		path, err = os.Getwd()
		if err != nil {
			return a.fail(err)
		}
	}
	result, err := session.Audit(session.AuditOptions{ProjectRoot: path, MaxFileSize: *maxMiB * 1024 * 1024})
	if err != nil {
		return a.fail(err)
	}
	if *jsonOutput {
		encoder := json.NewEncoder(a.Out)
		encoder.SetIndent("", "  ")
		if err := encoder.Encode(result); err != nil {
			return a.fail(err)
		}
	} else {
		for _, finding := range result.Findings {
			location := finding.Path
			if finding.Line > 0 {
				location = fmt.Sprintf("%s:%d", location, finding.Line)
			}
			fmt.Fprintf(a.Out, "%s %s %s: %s\n", strings.ToUpper(finding.Severity), finding.Kind, location, finding.Message)
		}
		fmt.Fprintf(a.Out, "Session audit: files=%d bytes=%d errors=%d warnings=%d\n", result.Files, result.Bytes, result.Errors, result.Warnings)
	}
	if result.HasFailures(*strict) {
		return 1
	}
	return 0
}

func (a *App) cmdSessionInit(args []string) int {
	fs := flag.NewFlagSet("session init", flag.ContinueOnError)
	fs.SetOutput(a.Err)
	projectPath := fs.String("project", "", "project root (defaults to current directory)")
	profileName := fs.String("profile", "", "profile to use for this command only")
	importExisting := fs.Bool("import-existing", false, "import existing sessions whose initial cwd is inside the project")
	if err := fs.Parse(args); err != nil {
		return 2
	}
	if fs.NArg() != 0 {
		fmt.Fprintln(a.Err, "usage: codexm session init [--project PATH] [--profile PROFILE] [--import-existing]")
		return 2
	}
	root := *projectPath
	if root == "" {
		var err error
		root, err = os.Getwd()
		if err != nil {
			return a.fail(err)
		}
	}
	managerHome, err := config.ManagerConfigDir()
	if err != nil {
		return a.fail(err)
	}
	profileHome := ""
	if *importExisting || *profileName != "" {
		_, profile, err := a.resolveSessionProfile(root, *profileName)
		if err != nil {
			return a.fail(err)
		}
		profileHome = profile.CodexHome
	}
	result, err := session.Init(session.InitOptions{
		ProjectRoot:    root,
		ProfileHome:    profileHome,
		ManagerHome:    managerHome,
		ImportExisting: *importExisting,
	})
	if err != nil {
		return a.fail(err)
	}
	verb := "Initialized"
	if !result.Created {
		verb = "Already initialized"
	}
	fmt.Fprintf(a.Out, "%s project session mirror %s (project_id=%s)\n", verb, root, result.Project.ProjectID)
	fmt.Fprintln(a.Out, "Warning: .codexm contains unencrypted session transcripts; review it before committing and prefer a private repository.")
	a.printSessionResult(result.Sync)
	return 0
}

func (a *App) cmdSessionSync(args []string, dryRun bool) int {
	name := "session sync"
	if dryRun {
		name = "session status"
	}
	fs := flag.NewFlagSet(name, flag.ContinueOnError)
	fs.SetOutput(a.Err)
	projectPath := fs.String("project", "", "project path (nearest initialized parent is used)")
	profileName := fs.String("profile", "", "profile to use for this command only")
	if err := fs.Parse(args); err != nil {
		return 2
	}
	if fs.NArg() != 0 {
		fmt.Fprintf(a.Err, "usage: codexm %s [--project PATH] [--profile PROFILE]\n", name)
		return 2
	}
	path := *projectPath
	if path == "" {
		var err error
		path, err = os.Getwd()
		if err != nil {
			return a.fail(err)
		}
	}
	root, _, err := session.FindProject(path)
	if err != nil {
		return a.fail(err)
	}
	_, profile, err := a.resolveSessionProfile(root, *profileName)
	if err != nil {
		return a.fail(err)
	}
	managerHome, err := config.ManagerConfigDir()
	if err != nil {
		return a.fail(err)
	}
	result, syncErr := session.Sync(session.Options{
		ProjectRoot: root,
		ProfileHome: profile.CodexHome,
		ManagerHome: managerHome,
		DryRun:      dryRun,
	})
	a.printSessionResult(result)
	if syncErr == nil && len(result.Actions) == 0 && len(result.Conflicts) == 0 {
		if dryRun {
			fmt.Fprintln(a.Out, "Project session mirror is synchronized; no changes pending.")
		} else {
			fmt.Fprintln(a.Out, "Project session mirror synchronized; no changes needed.")
		}
	}
	if syncErr != nil {
		return a.fail(syncErr)
	}
	return 0
}

func (a *App) cmdSessionResolve(args []string) int {
	fs := flag.NewFlagSet("session resolve", flag.ContinueOnError)
	fs.SetOutput(a.Err)
	projectPath := fs.String("project", "", "project path (nearest initialized parent is used)")
	profileName := fs.String("profile", "", "profile to use for this command only")
	use := fs.String("use", "", "winning copy: project or profile")
	if err := fs.Parse(args); err != nil {
		return 2
	}
	if fs.NArg() != 1 || (*use != "project" && *use != "profile") {
		fmt.Fprintln(a.Err, "usage: codexm session resolve [--project PATH] [--profile PROFILE] --use project|profile SESSION_ID")
		return 2
	}
	path := *projectPath
	if path == "" {
		var err error
		path, err = os.Getwd()
		if err != nil {
			return a.fail(err)
		}
	}
	root, _, err := session.FindProject(path)
	if err != nil {
		return a.fail(err)
	}
	_, profile, err := a.resolveSessionProfile(root, *profileName)
	if err != nil {
		return a.fail(err)
	}
	managerHome, err := config.ManagerConfigDir()
	if err != nil {
		return a.fail(err)
	}
	result, err := session.Resolve(session.ResolveOptions{
		ProjectRoot: root,
		ProfileHome: profile.CodexHome,
		ManagerHome: managerHome,
		SessionID:   fs.Arg(0),
		Use:         *use,
	})
	if err != nil {
		return a.fail(err)
	}
	fmt.Fprintf(a.Out, "Resolved session %s using %s copy.\nBackup: %s\n", result.SessionID, result.Winner, result.Backup)
	return 0
}

func (a *App) resolveSessionProfile(projectPath, explicit string) (string, config.Profile, error) {
	cfg, err := config.Load()
	if err != nil {
		return "", config.Profile{}, err
	}
	name := explicit
	if name == "" {
		var ok bool
		name, _, ok = config.ResolveProfile(cfg, projectPath)
		if !ok {
			return "", config.Profile{}, errors.New("no profile selected; provide --profile, bind this project, or set a default")
		}
	}
	profile, ok := cfg.Profiles[name]
	if !ok {
		return "", config.Profile{}, fmt.Errorf("profile %q does not exist", name)
	}
	return name, profile, nil
}

func (a *App) printSessionResult(result session.Result) {
	if len(result.Actions) == 0 && len(result.Conflicts) == 0 && result.Ignored == 0 {
		return
	}
	prefix := "codexm: session sync"
	if result.DryRun {
		prefix = "codexm: session status"
	}
	fmt.Fprintf(a.Out, "%s: import=%d export=%d update-project=%d update-profile=%d delete-project=%d delete-profile=%d ignored=%d\n",
		prefix, result.Imported, result.Exported, result.UpdatedProject, result.UpdatedProfile,
		result.DeletedProject, result.DeletedProfile, result.Ignored)
	for _, conflict := range result.Conflicts {
		fmt.Fprintf(a.Err, "session conflict %s: %s\n", conflict.SessionID, strings.TrimSpace(conflict.Reason))
	}
}
