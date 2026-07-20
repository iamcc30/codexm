package cli

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"os"
	"os/signal"
	"strings"

	"github.com/iamcc30/codexm/internal/appserver"
	"github.com/iamcc30/codexm/internal/config"
	"github.com/iamcc30/codexm/internal/dashboard"
	"github.com/iamcc30/codexm/internal/monitor"
	monitortui "github.com/iamcc30/codexm/internal/tui"
)

func (a *App) cmdDaemon(args []string) int {
	if len(args) == 0 {
		fmt.Fprintln(a.Err, "usage: codexm daemon start PROFILE|--all | status [PROFILE|--all] | stop PROFILE|--all [--force]")
		return 2
	}
	switch args[0] {
	case "start":
		return a.cmdDaemonStart(args[1:])
	case "status":
		return a.cmdDaemonStatus(args[1:])
	case "stop":
		return a.cmdDaemonStop(args[1:])
	default:
		fmt.Fprintf(a.Err, "unknown daemon command %q\n", args[0])
		return 2
	}
}

func (a *App) cmdDaemonStart(args []string) int {
	fs := flag.NewFlagSet("daemon start", flag.ContinueOnError)
	fs.SetOutput(a.Err)
	all := fs.Bool("all", false, "start all configured profiles")
	if err := fs.Parse(args); err != nil {
		return 2
	}
	cfg, manager, names, err := a.daemonTargets(fs.Args(), *all, false)
	if err != nil {
		return a.fail(err)
	}
	failed := false
	for _, name := range names {
		health, startErr := manager.Start(context.Background(), name, cfg.Profiles[name].CodexHome)
		if startErr != nil {
			fmt.Fprintf(a.Err, "[FAIL] %s: %v\n", name, startErr)
			failed = true
			continue
		}
		fmt.Fprintf(a.Out, "[OK] %s pid=%d endpoint=%s version=%s\n", name, health.PID, health.Endpoint, health.CodexVersion)
	}
	if failed {
		return 1
	}
	return 0
}

func (a *App) cmdDaemonStatus(args []string) int {
	fs := flag.NewFlagSet("daemon status", flag.ContinueOnError)
	fs.SetOutput(a.Err)
	all := fs.Bool("all", false, "show all configured profiles")
	if err := fs.Parse(args); err != nil {
		return 2
	}
	_, manager, names, err := a.daemonTargets(fs.Args(), *all, true)
	if err != nil {
		return a.fail(err)
	}
	failed := false
	for _, name := range names {
		health := manager.Status(context.Background(), name)
		state := "stopped"
		if health.Healthy {
			state = "healthy"
		} else if health.Running {
			state = "unhealthy"
		}
		fmt.Fprintf(a.Out, "%s\t%s\tpid=%d\tendpoint=%s\tversion=%s", name, state, health.PID, health.Endpoint, health.CodexVersion)
		if health.Error != "" {
			fmt.Fprintf(a.Out, "\terror=%s", singleLine(health.Error))
		}
		fmt.Fprintln(a.Out)
		if health.Running && !health.Healthy {
			failed = true
		}
	}
	if failed {
		return 1
	}
	return 0
}

func (a *App) cmdDaemonStop(args []string) int {
	fs := flag.NewFlagSet("daemon stop", flag.ContinueOnError)
	fs.SetOutput(a.Err)
	all := fs.Bool("all", false, "stop all configured profiles")
	force := fs.Bool("force", false, "stop even when threads are loaded")
	// Accept --force after PROFILE as documented, while keeping the standard
	// flag package for consistent error/help behavior.
	normalized := make([]string, 0, len(args))
	for _, arg := range args {
		if arg == "--force" {
			*force = true
			continue
		}
		normalized = append(normalized, arg)
	}
	if err := fs.Parse(normalized); err != nil {
		return 2
	}
	_, manager, names, err := a.daemonTargets(fs.Args(), *all, false)
	if err != nil {
		return a.fail(err)
	}
	failed := false
	for _, name := range names {
		if stopErr := manager.Stop(context.Background(), name, *force); stopErr != nil {
			fmt.Fprintf(a.Err, "[FAIL] %s: %v\n", name, stopErr)
			failed = true
			continue
		}
		fmt.Fprintf(a.Out, "[OK] %s stopped\n", name)
	}
	if failed {
		return 1
	}
	return 0
}

func (a *App) daemonTargets(args []string, all, allByDefault bool) (*config.Config, *appserver.Manager, []string, error) {
	if all && len(args) != 0 {
		return nil, nil, nil, errors.New("PROFILE and --all are mutually exclusive")
	}
	if len(args) > 1 {
		return nil, nil, nil, errors.New("expected one PROFILE or --all")
	}
	cfg, err := config.Load()
	if err != nil {
		return nil, nil, nil, err
	}
	managerHome, err := config.ManagerConfigDir()
	if err != nil {
		return nil, nil, nil, err
	}
	manager, err := appserver.NewManager(managerHome)
	if err != nil {
		return nil, nil, nil, err
	}
	var names []string
	switch {
	case all || (allByDefault && len(args) == 0):
		names = config.SortedProfileNames(cfg)
	case len(args) == 1:
		if _, ok := cfg.Profiles[args[0]]; !ok {
			return nil, nil, nil, fmt.Errorf("profile %q does not exist", args[0])
		}
		names = []string{args[0]}
	default:
		return nil, nil, nil, errors.New("provide PROFILE or --all")
	}
	return cfg, manager, names, nil
}

func (a *App) cmdDashboard(args []string) int {
	fs := flag.NewFlagSet("dashboard", flag.ContinueOnError)
	fs.SetOutput(a.Err)
	profile := fs.String("profile", "", "only show one profile")
	project := fs.String("project", "", "only show one project path")
	lan := fs.Bool("lan", false, "enable authenticated HTTPS access from the LAN")
	listen := fs.String("listen", "", "listen address (default 127.0.0.1:0)")
	noOpen := fs.Bool("no-open", false, "do not open a browser")
	rotate := fs.Bool("rotate-token", false, "rotate the dashboard access token")
	if err := fs.Parse(args); err != nil {
		return 2
	}
	if fs.NArg() != 0 {
		fmt.Fprintln(a.Err, "usage: codexm dashboard [--profile PROFILE] [--project PATH] [--lan] [--listen ADDR] [--no-open] [--rotate-token]")
		return 2
	}
	cfg, manager, filter, err := a.monitorInputs(*profile, *project)
	if err != nil {
		return a.fail(err)
	}
	ctx, cancel := signal.NotifyContext(context.Background(), os.Interrupt)
	defer cancel()
	store := monitor.NewStore()
	service := monitor.NewService(cfg, manager, store, monitor.ServiceOptions{Filter: filter, StartDaemons: true})
	go service.Run(ctx)
	server, err := dashboard.New(store, dashboard.Options{
		Listen: *listen, LAN: *lan, NoOpen: *noOpen, RotateToken: *rotate,
		RuntimeHome: manager.Home,
		OnReady: func(url string) {
			fmt.Fprintf(a.Out, "Dashboard: %s\n", url)
			if *lan {
				fmt.Fprintln(a.Out, "LAN HTTPS uses a self-signed certificate; trust it explicitly on each client device.")
			}
		},
	})
	if err != nil {
		return a.fail(err)
	}
	_, err = server.Run(ctx)
	if err != nil {
		return a.fail(err)
	}
	return 0
}

func (a *App) cmdUI(args []string) int {
	fs := flag.NewFlagSet("ui", flag.ContinueOnError)
	fs.SetOutput(a.Err)
	profile := fs.String("profile", "", "only show one profile")
	project := fs.String("project", "", "only show one project path")
	if err := fs.Parse(args); err != nil {
		return 2
	}
	if fs.NArg() != 0 {
		fmt.Fprintln(a.Err, "usage: codexm ui [--profile PROFILE] [--project PATH]")
		return 2
	}
	cfg, manager, filter, err := a.monitorInputs(*profile, *project)
	if err != nil {
		return a.fail(err)
	}
	ctx, cancel := signal.NotifyContext(context.Background(), os.Interrupt)
	defer cancel()
	store := monitor.NewStore()
	service := monitor.NewService(cfg, manager, store, monitor.ServiceOptions{Filter: filter, StartDaemons: true})
	go service.Run(ctx)
	if err := monitortui.Run(ctx, store, a.Out); err != nil && !errors.Is(err, context.Canceled) {
		return a.fail(err)
	}
	return 0
}

func (a *App) monitorInputs(profile, project string) (*config.Config, *appserver.Manager, monitor.Filter, error) {
	cfg, err := config.Load()
	if err != nil {
		return nil, nil, monitor.Filter{}, err
	}
	if profile != "" {
		if _, ok := cfg.Profiles[profile]; !ok {
			return nil, nil, monitor.Filter{}, fmt.Errorf("profile %q does not exist", profile)
		}
	}
	if project != "" {
		project, err = config.NormalizePath(project)
		if err != nil {
			return nil, nil, monitor.Filter{}, err
		}
	}
	home, err := config.ManagerConfigDir()
	if err != nil {
		return nil, nil, monitor.Filter{}, err
	}
	manager, err := appserver.NewManager(home)
	if err != nil {
		return nil, nil, monitor.Filter{}, err
	}
	return cfg, manager, monitor.Filter{Profile: profile, Project: project}, nil
}

func supportsManagedRemote(args []string) bool {
	command := codexSubcommand(args)
	switch command {
	case "", "resume", "fork", "archive", "delete", "unarchive":
		return true
	default:
		return false
	}
}

func managedRemoteArgs(endpoint string, child []string) []string {
	args := []string{
		"--remote", endpoint,
		"--remote-auth-token-env", appserver.RemoteTokenEnv,
	}
	return append(args, child...)
}

func hasRemoteArgument(args []string) bool {
	for _, arg := range args {
		if arg == "--remote" || strings.HasPrefix(arg, "--remote=") {
			return true
		}
	}
	return false
}

func codexSubcommand(args []string) string {
	commands := map[string]bool{
		"exec": true, "review": true, "login": true, "logout": true, "mcp": true,
		"plugin": true, "mcp-server": true, "app-server": true, "remote-control": true,
		"app": true, "completion": true, "update": true, "doctor": true, "sandbox": true,
		"debug": true, "apply": true, "resume": true, "archive": true, "delete": true,
		"unarchive": true, "fork": true, "cloud": true, "exec-server": true,
		"features": true, "help": true,
	}
	takesValue := map[string]bool{
		"-c": true, "--config": true, "--enable": true, "--disable": true,
		"-i": true, "--image": true, "-m": true, "--model": true,
		"--local-provider": true, "-p": true, "--profile": true,
		"-s": true, "--sandbox": true, "-C": true, "--cd": true,
		"--add-dir": true, "-a": true, "--ask-for-approval": true,
		"--remote": true, "--remote-auth-token-env": true,
	}
	skip := false
	for _, arg := range args {
		if skip {
			skip = false
			continue
		}
		if takesValue[arg] {
			skip = true
			continue
		}
		if strings.HasPrefix(arg, "-") {
			continue
		}
		if commands[arg] {
			return arg
		}
		// A non-command positional value is the initial prompt for interactive
		// Codex, not a subcommand.
		return ""
	}
	return ""
}
