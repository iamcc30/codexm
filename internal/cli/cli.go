package cli

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"sort"
	"strings"

	"github.com/iamcc30/codexm/internal/appserver"
	"github.com/iamcc30/codexm/internal/codex"
	"github.com/iamcc30/codexm/internal/config"
	"github.com/iamcc30/codexm/internal/sharedmcp"
)

type App struct {
	Out     io.Writer
	Err     io.Writer
	Version string
}

func New(version string) *App {
	return &App{Out: os.Stdout, Err: os.Stderr, Version: version}
}

func (a *App) Run(args []string) int {
	if len(args) == 0 {
		a.printHelp()
		return 0
	}
	switch args[0] {
	case "help", "-h", "--help":
		a.printHelp()
		return 0
	case "version", "--version", "-v":
		fmt.Fprintf(a.Out, "codexm %s\n", a.Version)
		return 0
	case "init":
		return a.cmdInit(args[1:])
	case "add":
		return a.cmdAdd(args[1:])
	case "remove", "rm":
		return a.cmdRemove(args[1:])
	case "list", "ls":
		return a.cmdList(args[1:])
	case "show":
		return a.cmdShow(args[1:])
	case "default":
		return a.cmdDefault(args[1:])
	case "bind":
		return a.cmdBind(args[1:])
	case "unbind":
		return a.cmdUnbind(args[1:])
	case "current":
		return a.cmdCurrent(args[1:])
	case "login":
		return a.cmdLogin(args[1:])
	case "logout":
		return a.cmdLogout(args[1:])
	case "status":
		return a.cmdStatus(args[1:])
	case "mcp":
		return a.cmdMCP(args[1:])
	case "session":
		return a.cmdSession(args[1:])
	case "daemon":
		return a.cmdDaemon(args[1:])
	case "dashboard":
		return a.cmdDashboard(args[1:])
	case "ui":
		return a.cmdUI(args[1:])
	case "run", "use":
		return a.cmdRun(args[1:])
	case "shell":
		return a.cmdShell(args[1:])
	case "doctor":
		return a.cmdDoctor(args[1:])
	case "config-path":
		return a.cmdConfigPath(args[1:])
	default:
		fmt.Fprintf(a.Err, "unknown command %q\n\n", args[0])
		a.printHelp()
		return 2
	}
}

func (a *App) printHelp() {
	fmt.Fprint(a.Out, `codexm - multi-account manager for the OpenAI Codex CLI

Usage:
  codexm <command> [options]

Commands:
  init                         Initialize codexm storage
  add [options] NAME           Add an isolated Codex account profile
  remove [options] NAME        Remove a profile
  list [--status]              List profiles
  show NAME                    Show one profile
  default [NAME|--clear]       Get or set the default profile
  bind PROFILE [PATH]          Bind a project directory to a profile
  unbind [PATH]                Remove a project binding
  current [PATH]               Show the profile selected for a path
  login [--device] PROFILE     Sign in to a profile
  logout PROFILE               Sign out of a profile
  status [PROFILE|--all]       Check login status
  mcp <command>                Manage MCP servers shared by profiles
  session <command>            Manage portable project Codex sessions
  daemon <command>             Manage profile app-server daemons
  dashboard [options]          Open the read-only web monitor
  ui [options]                 Open the read-only terminal monitor
  run [options] [PROFILE] -- [CODEX_ARGS...]
                               Run Codex using a selected/automatic profile
  shell PROFILE                Open a shell with CODEX_HOME selected
  doctor                       Diagnose installation and profiles
  config-path                  Print codexm's config file path
  version                      Print version

Typical flow:
  codexm add account1
  codexm login account1
  codexm bind account1 ~/Projects/project1
  cd ~/Projects/project1 && codexm run

Environment:
  CODEXM_HOME                  Override codexm config directory
  CODEXM_PROFILES_HOME         Override default profile storage directory
  CODEXM_CODEX_BIN             Override Codex executable path/name
`)
}

func (a *App) cmdInit(args []string) int {
	fs := flag.NewFlagSet("init", flag.ContinueOnError)
	fs.SetOutput(a.Err)
	if err := fs.Parse(args); err != nil {
		return 2
	}
	if err := config.Update(func(*config.Config) error { return nil }); err != nil {
		return a.fail(err)
	}
	path, _ := config.ConfigPath()
	fmt.Fprintf(a.Out, "Initialized codexm at %s\n", path)
	return 0
}

func (a *App) cmdAdd(args []string) int {
	fs := flag.NewFlagSet("add", flag.ContinueOnError)
	fs.SetOutput(a.Err)
	home := fs.String("home", "", "custom CODEX_HOME for this profile")
	description := fs.String("description", "", "profile description")
	bindPath := fs.String("bind", "", "bind a project path immediately")
	store := fs.String("credential-store", "file", "file, auto, or keyring")
	cloneConfig := fs.String("clone-config", "", "copy config.toml from another profile (never credentials)")
	if err := fs.Parse(args); err != nil {
		return 2
	}
	if fs.NArg() != 1 {
		fmt.Fprintln(a.Err, "usage: codexm add [options] NAME")
		return 2
	}
	name := fs.Arg(0)
	if err := config.ValidateProfileName(name); err != nil {
		return a.fail(err)
	}
	profileHome := ""
	err := config.Update(func(cfg *config.Config) error {
		if _, exists := cfg.Profiles[name]; exists {
			return fmt.Errorf("profile %q already exists", name)
		}
		var err error
		profileHome = *home
		if profileHome == "" {
			profileHome, err = config.DefaultProfileHome(name)
		} else {
			profileHome, err = config.NormalizePath(profileHome)
		}
		if err != nil {
			return err
		}
		if *cloneConfig != "" {
			source, ok := cfg.Profiles[*cloneConfig]
			if !ok {
				return fmt.Errorf("source profile %q does not exist", *cloneConfig)
			}
			if err := os.MkdirAll(profileHome, 0o700); err != nil {
				return err
			}
			src := filepath.Join(source.CodexHome, "config.toml")
			dst := filepath.Join(profileHome, "config.toml")
			if data, readErr := os.ReadFile(src); readErr == nil {
				if writeErr := os.WriteFile(dst, data, 0o600); writeErr != nil {
					return writeErr
				}
			} else if !errors.Is(readErr, os.ErrNotExist) {
				return readErr
			}
		}
		if err := codex.EnsureProfileHome(profileHome, *store); err != nil {
			return err
		}
		newProfile := config.NewProfile(profileHome, *description)
		if _, err := a.syncMCPProfile(newProfile, true); err != nil {
			return err
		}
		cfg.Profiles[name] = newProfile
		if cfg.DefaultProfile == "" {
			cfg.DefaultProfile = name
		}
		if *bindPath != "" {
			normalized, err := config.NormalizePath(*bindPath)
			if err != nil {
				return err
			}
			cfg.Bindings[normalized] = name
		}
		return nil
	})
	if err != nil {
		return a.fail(err)
	}
	fmt.Fprintf(a.Out, "Added profile %q\nCODEX_HOME: %s\n", name, profileHome)
	if *store == "keyring" {
		fmt.Fprintln(a.Out, "Warning: keyring storage can be shared outside this CODEX_HOME; file storage is safer for account isolation.")
	}
	return 0
}

func (a *App) cmdRemove(args []string) int {
	fs := flag.NewFlagSet("remove", flag.ContinueOnError)
	fs.SetOutput(a.Err)
	deleteHome := fs.Bool("delete-home", false, "delete the profile CODEX_HOME directory")
	yes := fs.Bool("yes", false, "skip confirmation when deleting profile home")
	if err := fs.Parse(args); err != nil {
		return 2
	}
	if fs.NArg() != 1 {
		fmt.Fprintln(a.Err, "usage: codexm remove [--delete-home --yes] NAME")
		return 2
	}
	name := fs.Arg(0)
	if *deleteHome && !*yes {
		return a.fail(errors.New("refusing to delete profile home without --yes"))
	}
	var profile config.Profile
	err := config.Update(func(cfg *config.Config) error {
		var ok bool
		profile, ok = cfg.Profiles[name]
		if !ok {
			return fmt.Errorf("profile %q does not exist", name)
		}
		delete(cfg.Profiles, name)
		config.RemoveBindingsForProfile(cfg, name)
		if cfg.DefaultProfile == name {
			cfg.DefaultProfile = ""
			names := config.SortedProfileNames(cfg)
			if len(names) > 0 {
				cfg.DefaultProfile = names[0]
			}
		}
		return nil
	})
	if err != nil {
		return a.fail(err)
	}
	if *deleteHome {
		if err := os.RemoveAll(profile.CodexHome); err != nil {
			return a.fail(err)
		}
	}
	fmt.Fprintf(a.Out, "Removed profile %q\n", name)
	return 0
}

func (a *App) cmdList(args []string) int {
	fs := flag.NewFlagSet("list", flag.ContinueOnError)
	fs.SetOutput(a.Err)
	withStatus := fs.Bool("status", false, "include login status")
	if err := fs.Parse(args); err != nil {
		return 2
	}
	cfg, err := config.Load()
	if err != nil {
		return a.fail(err)
	}
	names := config.SortedProfileNames(cfg)
	if len(names) == 0 {
		fmt.Fprintln(a.Out, "No profiles. Add one with: codexm add NAME")
		return 0
	}
	var runner *codex.Runner
	if *withStatus {
		runner, err = codex.Find()
		if err != nil {
			return a.fail(err)
		}
	}
	fmt.Fprintln(a.Out, "PROFILE\tDEFAULT\tSTATUS\tCODEX_HOME\tDESCRIPTION")
	for _, name := range names {
		p := cfg.Profiles[name]
		def := ""
		if cfg.DefaultProfile == name {
			def = "*"
		}
		status := "-"
		if *withStatus {
			_, code, capErr := runner.Capture(p.CodexHome, "", []string{"login", "status"})
			if capErr != nil {
				status = "error"
			} else if code == 0 {
				status = "logged-in"
			} else {
				status = "logged-out"
			}
		}
		fmt.Fprintf(a.Out, "%s\t%s\t%s\t%s\t%s\n", name, def, status, p.CodexHome, p.Description)
	}
	return 0
}

func (a *App) cmdShow(args []string) int {
	if len(args) != 1 {
		fmt.Fprintln(a.Err, "usage: codexm show NAME")
		return 2
	}
	cfg, err := config.Load()
	if err != nil {
		return a.fail(err)
	}
	p, ok := cfg.Profiles[args[0]]
	if !ok {
		return a.fail(fmt.Errorf("profile %q does not exist", args[0]))
	}
	fmt.Fprintf(a.Out, "Profile: %s\nDefault: %t\nCODEX_HOME: %s\nDescription: %s\nCreated: %s\n", args[0], cfg.DefaultProfile == args[0], p.CodexHome, p.Description, p.CreatedAt)
	fmt.Fprintln(a.Out, "Bindings:")
	var roots []string
	for root, profile := range cfg.Bindings {
		if profile == args[0] {
			roots = append(roots, root)
		}
	}
	sort.Strings(roots)
	if len(roots) == 0 {
		fmt.Fprintln(a.Out, "  (none)")
	} else {
		for _, root := range roots {
			fmt.Fprintf(a.Out, "  %s\n", root)
		}
	}
	if len(p.ExcludedMCPServers) == 0 {
		fmt.Fprintln(a.Out, "Excluded shared MCP servers: (none)")
	} else {
		fmt.Fprintf(a.Out, "Excluded shared MCP servers: %s\n", strings.Join(p.ExcludedMCPServers, ", "))
	}
	return 0
}

func (a *App) cmdDefault(args []string) int {
	fs := flag.NewFlagSet("default", flag.ContinueOnError)
	fs.SetOutput(a.Err)
	clear := fs.Bool("clear", false, "clear default profile")
	if err := fs.Parse(args); err != nil {
		return 2
	}
	if *clear {
		if fs.NArg() != 0 {
			return a.fail(errors.New("--clear does not accept a profile name"))
		}
		if err := config.Update(func(cfg *config.Config) error {
			cfg.DefaultProfile = ""
			return nil
		}); err != nil {
			return a.fail(err)
		}
		fmt.Fprintln(a.Out, "Default profile cleared")
		return 0
	}
	if fs.NArg() == 0 {
		cfg, err := config.Load()
		if err != nil {
			return a.fail(err)
		}
		if cfg.DefaultProfile == "" {
			fmt.Fprintln(a.Out, "(none)")
		} else {
			fmt.Fprintln(a.Out, cfg.DefaultProfile)
		}
		return 0
	}
	if fs.NArg() != 1 {
		fmt.Fprintln(a.Err, "usage: codexm default [NAME|--clear]")
		return 2
	}
	name := fs.Arg(0)
	if err := config.Update(func(cfg *config.Config) error {
		if _, ok := cfg.Profiles[name]; !ok {
			return fmt.Errorf("profile %q does not exist", name)
		}
		cfg.DefaultProfile = name
		return nil
	}); err != nil {
		return a.fail(err)
	}
	fmt.Fprintf(a.Out, "Default profile set to %q\n", name)
	return 0
}

func (a *App) cmdBind(args []string) int {
	if len(args) < 1 || len(args) > 2 {
		fmt.Fprintln(a.Err, "usage: codexm bind PROFILE [PATH]")
		return 2
	}
	profile := args[0]
	path := "."
	if len(args) == 2 {
		path = args[1]
	}
	normalized, err := config.NormalizePath(path)
	if err != nil {
		return a.fail(err)
	}
	info, err := os.Stat(normalized)
	if err != nil {
		return a.fail(err)
	}
	if !info.IsDir() {
		return a.fail(fmt.Errorf("binding path is not a directory: %s", normalized))
	}
	if err := config.Update(func(cfg *config.Config) error {
		if _, ok := cfg.Profiles[profile]; !ok {
			return fmt.Errorf("profile %q does not exist", profile)
		}
		cfg.Bindings[normalized] = profile
		return nil
	}); err != nil {
		return a.fail(err)
	}
	fmt.Fprintf(a.Out, "Bound %s -> %s\n", normalized, profile)
	return 0
}

func (a *App) cmdUnbind(args []string) int {
	if len(args) > 1 {
		fmt.Fprintln(a.Err, "usage: codexm unbind [PATH]")
		return 2
	}
	path := "."
	if len(args) == 1 {
		path = args[0]
	}
	normalized, err := config.NormalizePath(path)
	if err != nil {
		return a.fail(err)
	}
	if err := config.Update(func(cfg *config.Config) error {
		if _, ok := cfg.Bindings[normalized]; !ok {
			return fmt.Errorf("no exact binding exists for %s", normalized)
		}
		delete(cfg.Bindings, normalized)
		return nil
	}); err != nil {
		return a.fail(err)
	}
	fmt.Fprintf(a.Out, "Unbound %s\n", normalized)
	return 0
}

func (a *App) cmdCurrent(args []string) int {
	if len(args) > 1 {
		fmt.Fprintln(a.Err, "usage: codexm current [PATH]")
		return 2
	}
	path := "."
	if len(args) == 1 {
		path = args[0]
	}
	cfg, err := config.Load()
	if err != nil {
		return a.fail(err)
	}
	profile, source, ok := config.ResolveProfile(cfg, path)
	if !ok {
		fmt.Fprintln(a.Out, "(none)")
		return 1
	}
	fmt.Fprintf(a.Out, "%s\t%s\n", profile, source)
	return 0
}

func (a *App) cmdLogin(args []string) int {
	fs := flag.NewFlagSet("login", flag.ContinueOnError)
	fs.SetOutput(a.Err)
	device := fs.Bool("device", false, "use device-code login")
	if err := fs.Parse(args); err != nil {
		return 2
	}
	if fs.NArg() != 1 {
		fmt.Fprintln(a.Err, "usage: codexm login [--device] PROFILE")
		return 2
	}
	p, ok, err := a.profile(fs.Arg(0))
	if err != nil {
		return a.fail(err)
	}
	if !ok {
		return a.fail(fmt.Errorf("profile %q does not exist", fs.Arg(0)))
	}
	runner, err := codex.Find()
	if err != nil {
		return a.fail(err)
	}
	cmdArgs := []string{"login"}
	if *device {
		cmdArgs = append(cmdArgs, "--device-auth")
	}
	fmt.Fprintf(a.Out, "Using profile %q (%s)\n", fs.Arg(0), p.CodexHome)
	return a.runAndCode(runner, p.CodexHome, "", cmdArgs)
}

func (a *App) cmdLogout(args []string) int {
	if len(args) != 1 {
		fmt.Fprintln(a.Err, "usage: codexm logout PROFILE")
		return 2
	}
	p, ok, err := a.profile(args[0])
	if err != nil {
		return a.fail(err)
	}
	if !ok {
		return a.fail(fmt.Errorf("profile %q does not exist", args[0]))
	}
	runner, err := codex.Find()
	if err != nil {
		return a.fail(err)
	}
	return a.runAndCode(runner, p.CodexHome, "", []string{"logout"})
}

func (a *App) cmdStatus(args []string) int {
	fs := flag.NewFlagSet("status", flag.ContinueOnError)
	fs.SetOutput(a.Err)
	all := fs.Bool("all", false, "check all profiles")
	if err := fs.Parse(args); err != nil {
		return 2
	}
	cfg, err := config.Load()
	if err != nil {
		return a.fail(err)
	}
	runner, err := codex.Find()
	if err != nil {
		return a.fail(err)
	}
	var names []string
	if *all {
		if fs.NArg() != 0 {
			return a.fail(errors.New("--all does not accept a profile name"))
		}
		names = config.SortedProfileNames(cfg)
	} else if fs.NArg() == 1 {
		names = []string{fs.Arg(0)}
	} else if fs.NArg() == 0 {
		cwd, _ := os.Getwd()
		name, _, ok := config.ResolveProfile(cfg, cwd)
		if !ok {
			return a.fail(errors.New("no profile selected; provide a profile, bind this project, or set a default"))
		}
		names = []string{name}
	} else {
		fmt.Fprintln(a.Err, "usage: codexm status [PROFILE|--all]")
		return 2
	}
	overall := 0
	for _, name := range names {
		p, ok := cfg.Profiles[name]
		if !ok {
			fmt.Fprintf(a.Err, "%s: profile does not exist\n", name)
			overall = 1
			continue
		}
		output, code, capErr := runner.Capture(p.CodexHome, "", []string{"login", "status"})
		if capErr != nil {
			fmt.Fprintf(a.Err, "%s: %v\n", name, capErr)
			overall = 1
			continue
		}
		state := "logged-out"
		if code == 0 {
			state = "logged-in"
		} else {
			overall = 1
		}
		if output != "" {
			fmt.Fprintf(a.Out, "%s: %s — %s\n", name, state, singleLine(output))
		} else {
			fmt.Fprintf(a.Out, "%s: %s\n", name, state)
		}
	}
	return overall
}

func (a *App) cmdDoctor(args []string) int {
	if len(args) != 0 {
		fmt.Fprintln(a.Err, "usage: codexm doctor")
		return 2
	}
	issues := 0
	path, err := config.ConfigPath()
	if err != nil {
		fmt.Fprintf(a.Err, "[FAIL] config path: %v\n", err)
		issues++
	} else {
		fmt.Fprintf(a.Out, "[OK] config path: %s\n", path)
	}
	cfg, err := config.Load()
	if err != nil {
		fmt.Fprintf(a.Err, "[FAIL] config load: %v\n", err)
		return 1
	}
	runner, err := codex.Find()
	if err != nil {
		fmt.Fprintf(a.Err, "[FAIL] Codex CLI: %v\n", err)
		issues++
	} else {
		output, code, capErr := runner.Capture("", "", []string{"--version"})
		if capErr != nil || code != 0 {
			fmt.Fprintf(a.Err, "[FAIL] Codex CLI executable: %s\n", runner.Executable)
			issues++
		} else {
			fmt.Fprintf(a.Out, "[OK] Codex CLI: %s (%s)\n", runner.Executable, singleLine(output))
		}
	}
	if len(cfg.Profiles) == 0 {
		fmt.Fprintln(a.Out, "[WARN] no profiles configured")
	}
	managerHome, managerHomeErr := config.ManagerConfigDir()
	var daemonManager *appserver.Manager
	if managerHomeErr != nil {
		fmt.Fprintf(a.Err, "[FAIL] app-server runtime path: %v\n", managerHomeErr)
		issues++
	} else if runner != nil {
		daemonManager, err = appserver.NewManager(managerHome)
		if err != nil {
			fmt.Fprintf(a.Err, "[FAIL] app-server manager: %v\n", err)
			issues++
		} else {
			fmt.Fprintf(a.Out, "[OK] private runtime path: %s\n", filepath.Join(managerHome, "runtime"))
		}
	}
	sharedPath, pathErr := config.SharedMCPConfigPath()
	if pathErr != nil {
		fmt.Fprintf(a.Err, "[FAIL] shared MCP config path: %v\n", pathErr)
		issues++
	} else if servers, serverErr := sharedmcp.ServerNames(sharedPath); serverErr != nil {
		fmt.Fprintf(a.Err, "[FAIL] shared MCP config: %v\n", serverErr)
		issues++
	} else {
		fmt.Fprintf(a.Out, "[OK] shared MCP config: %s (%d server(s))\n", sharedPath, len(servers))
	}
	for _, name := range config.SortedProfileNames(cfg) {
		p := cfg.Profiles[name]
		info, statErr := os.Stat(p.CodexHome)
		if statErr != nil || !info.IsDir() {
			fmt.Fprintf(a.Err, "[FAIL] %s: CODEX_HOME missing: %s\n", name, p.CodexHome)
			issues++
			continue
		}
		configToml := filepath.Join(p.CodexHome, "config.toml")
		stores, readErr := codex.ReadCredentialStores(configToml)
		if readErr != nil {
			fmt.Fprintf(a.Err, "[FAIL] %s: cannot read credential stores from %s: %v\n", name, configToml, readErr)
			issues++
			continue
		}
		if stores.CLI == "file" {
			fmt.Fprintf(a.Out, "[OK] %s: isolated file credential store\n", name)
		} else {
			fmt.Fprintf(a.Out, "[WARN] %s: credential store is %q, not explicitly file-based\n", name, stores.CLI)
		}
		if stores.MCPOAuth == "file" {
			fmt.Fprintf(a.Out, "[OK] %s: isolated MCP OAuth credential store\n", name)
		} else {
			fmt.Fprintf(a.Out, "[WARN] %s: MCP OAuth credential store is %q, not explicitly file-based\n", name, stores.MCPOAuth)
		}
		if daemonManager != nil {
			if daemonManager.Supported(p.CodexHome) {
				fmt.Fprintf(a.Out, "[OK] %s: Codex supports managed app-server remote mode (experimental)\n", name)
			} else {
				fmt.Fprintf(a.Out, "[WARN] %s: installed Codex lacks managed app-server remote capability; runs stay unmanaged\n", name)
			}
			health := daemonManager.Status(context.Background(), name)
			switch {
			case health.Healthy:
				fmt.Fprintf(a.Out, "[OK] %s: app-server healthy (pid=%d, %s)\n", name, health.PID, health.Endpoint)
			case health.Running:
				fmt.Fprintf(a.Err, "[FAIL] %s: app-server unhealthy: %s\n", name, health.Error)
				issues++
			default:
				fmt.Fprintf(a.Out, "[WARN] %s: app-server is not running (starts on demand)\n", name)
			}
		}
		if result, syncErr := a.syncMCPProfile(p, false); syncErr != nil {
			fmt.Fprintf(a.Err, "[FAIL] %s: shared MCP sync check: %v\n", name, syncErr)
			issues++
		} else if result.Changed {
			fmt.Fprintf(a.Out, "[WARN] %s: shared MCP configuration is out of sync; run codexm mcp sync %s\n", name, name)
		} else {
			fmt.Fprintf(a.Out, "[OK] %s: shared MCP configuration synchronized\n", name)
		}
	}
	for root, profile := range cfg.Bindings {
		if _, ok := cfg.Profiles[profile]; !ok {
			fmt.Fprintf(a.Err, "[FAIL] binding %s references missing profile %s\n", root, profile)
			issues++
		}
	}
	if issues > 0 {
		fmt.Fprintf(a.Err, "Doctor found %d issue(s).\n", issues)
		return 1
	}
	fmt.Fprintln(a.Out, "Doctor found no blocking issues.")
	return 0
}

func (a *App) cmdConfigPath(args []string) int {
	if len(args) != 0 {
		fmt.Fprintln(a.Err, "usage: codexm config-path")
		return 2
	}
	path, err := config.ConfigPath()
	if err != nil {
		return a.fail(err)
	}
	fmt.Fprintln(a.Out, path)
	return 0
}

func (a *App) profile(name string) (config.Profile, bool, error) {
	cfg, err := config.Load()
	if err != nil {
		return config.Profile{}, false, err
	}
	p, ok := cfg.Profiles[name]
	return p, ok, nil
}

func (a *App) fail(err error) int {
	fmt.Fprintf(a.Err, "error: %v\n", err)
	return 1
}

func singleLine(s string) string {
	return strings.Join(strings.Fields(s), " ")
}
