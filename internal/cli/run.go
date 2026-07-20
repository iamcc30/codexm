package cli

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"io"
	"os"
	"os/exec"
	"runtime"

	"github.com/iamcc30/codexm/internal/appserver"
	"github.com/iamcc30/codexm/internal/codex"
	"github.com/iamcc30/codexm/internal/config"
	"github.com/iamcc30/codexm/internal/session"
)

func (a *App) cmdRun(args []string) int {
	before, passthrough := splitDoubleDash(args)
	fs := flag.NewFlagSet("run", flag.ContinueOnError)
	fs.SetOutput(a.Err)
	project := fs.String("project", "", "working directory used for profile resolution and Codex")
	unmanaged := fs.Bool("unmanaged", false, "run Codex directly without the managed app-server")
	if err := fs.Parse(before); err != nil {
		return 2
	}
	if fs.NArg() > 1 {
		fmt.Fprintln(a.Err, "usage: codexm run [--project PATH] [PROFILE] -- [CODEX_ARGS...]")
		return 2
	}
	cwd := *project
	var err error
	if cwd == "" {
		cwd, err = os.Getwd()
	} else {
		cwd, err = config.NormalizePath(cwd)
	}
	if err != nil {
		return a.fail(err)
	}
	cfg, err := config.Load()
	if err != nil {
		return a.fail(err)
	}
	profileName, source := "", "explicit"
	if fs.NArg() == 1 {
		profileName = fs.Arg(0)
	} else {
		var ok bool
		profileName, source, ok = config.ResolveProfile(cfg, cwd)
		if !ok {
			return a.fail(errors.New("no profile selected; provide one, bind this project, or set a default"))
		}
	}
	p, ok := cfg.Profiles[profileName]
	if !ok {
		return a.fail(fmt.Errorf("profile %q does not exist", profileName))
	}
	if _, err := a.syncMCPProfile(p, true); err != nil {
		return a.fail(err)
	}
	managerHome, err := config.ManagerConfigDir()
	if err != nil {
		return a.fail(err)
	}
	sessionRoot, _, sessionErr := session.FindProject(cwd)
	sessionEnabled := sessionErr == nil
	if sessionErr != nil && !errors.Is(sessionErr, session.ErrNotInitialized) {
		return a.fail(sessionErr)
	}
	var beforeSessions map[string]bool
	if sessionEnabled {
		if result, err := session.Sync(session.Options{
			ProjectRoot: sessionRoot, ProfileHome: p.CodexHome, ManagerHome: managerHome, OnlyEligible: true,
		}); err != nil {
			a.printSessionResult(result)
			return a.fail(err)
		} else {
			a.printSessionResult(result)
		}
		beforeSessions, err = session.SnapshotProfile(p.CodexHome, sessionRoot)
		if err != nil {
			return a.fail(err)
		}
	}
	runner, err := codex.Find()
	if err != nil {
		return a.fail(err)
	}
	runArgs := passthrough
	var extraEnv map[string]string
	if hasRemoteArgument(passthrough) && !*unmanaged {
		return a.fail(errors.New("custom --remote requires --unmanaged to avoid an ambiguous managed endpoint"))
	}
	if !*unmanaged && supportsManagedRemote(passthrough) {
		manager, managerErr := appserver.NewManager(managerHome)
		if managerErr != nil {
			return a.fail(managerErr)
		}
		if manager.Supported(p.CodexHome) {
			health, startErr := manager.Start(context.Background(), profileName, p.CodexHome)
			if startErr != nil {
				return a.fail(fmt.Errorf("managed app-server failed: %w (use --unmanaged to run directly)", startErr))
			}
			token, readErr := os.ReadFile(health.TokenFile)
			if readErr != nil {
				return a.fail(fmt.Errorf("read managed app-server token: %w", readErr))
			}
			runArgs = managedRemoteArgs(health.Endpoint, passthrough)
			extraEnv = map[string]string{appserver.RemoteTokenEnv: string(token), appserver.ManagedRemoteEnv: "1"}
			fmt.Fprintf(a.Out, "codexm: managed app-server pid=%d endpoint=%s\n", health.PID, health.Endpoint)
		} else {
			fmt.Fprintln(a.Out, "codexm: installed Codex does not support managed remote mode; running unmanaged")
		}
	} else {
		fmt.Fprintln(a.Out, "codexm: unmanaged task (remote mode disabled or unsupported for this subcommand)")
	}
	fmt.Fprintf(a.Out, "codexm: profile=%s source=%s CODEX_HOME=%s\n", profileName, source, p.CodexHome)
	runCode := codexExitCode(runner.RunWithEnv(p.CodexHome, cwd, runArgs, extraEnv), a.Err)
	if sessionEnabled {
		afterSessions, snapshotErr := session.SnapshotProfile(p.CodexHome, sessionRoot)
		var postErr error
		if snapshotErr != nil {
			postErr = snapshotErr
		} else {
			result, syncErr := session.Sync(session.Options{
				ProjectRoot: sessionRoot, ProfileHome: p.CodexHome, ManagerHome: managerHome,
				Eligible: session.NewSessions(beforeSessions, afterSessions), OnlyEligible: true,
			})
			a.printSessionResult(result)
			postErr = syncErr
		}
		if postErr != nil {
			fmt.Fprintf(a.Err, "error: post-run session sync: %v\n", postErr)
			if runCode == 0 {
				runCode = 1
			}
		}
	}
	return runCode
}

func codexExitCode(err error, errorOutput io.Writer) int {
	if err == nil {
		return 0
	}
	var exitErr *codex.ExitError
	if errors.As(err, &exitErr) {
		return exitErr.Code
	}
	fmt.Fprintf(errorOutput, "error: %v\n", err)
	return 1
}

func (a *App) cmdShell(args []string) int {
	if len(args) != 1 {
		fmt.Fprintln(a.Err, "usage: codexm shell PROFILE")
		return 2
	}
	p, ok, err := a.profile(args[0])
	if err != nil {
		return a.fail(err)
	}
	if !ok {
		return a.fail(fmt.Errorf("profile %q does not exist", args[0]))
	}
	if _, err := a.syncMCPProfile(p, true); err != nil {
		return a.fail(err)
	}
	shell, shellArgs := "", []string{}
	if runtime.GOOS == "windows" {
		shell = os.Getenv("COMSPEC")
		if shell == "" {
			shell = "cmd.exe"
		}
	} else {
		shell = os.Getenv("SHELL")
		if shell == "" {
			shell = "/bin/sh"
		}
		shellArgs = []string{"-i"}
	}
	cmd := exec.Command(shell, shellArgs...)
	cmd.Env = codex.EnvWithCodexHome(os.Environ(), p.CodexHome)
	cmd.Stdin, cmd.Stdout, cmd.Stderr = os.Stdin, os.Stdout, os.Stderr
	fmt.Fprintf(a.Out, "Opening shell for profile %q; CODEX_HOME=%s\n", args[0], p.CodexHome)
	fmt.Fprintln(a.Out, "Warning: codexm shell bypasses automatic project session synchronization; run `codexm session sync` afterward.")
	if err := cmd.Run(); err != nil {
		return a.fail(err)
	}
	return 0
}

func (a *App) runAndCode(runner *codex.Runner, home, cwd string, args []string) int {
	err := runner.Run(home, cwd, args)
	if err == nil {
		return 0
	}
	var exitErr *codex.ExitError
	if errors.As(err, &exitErr) {
		return exitErr.Code
	}
	return a.fail(err)
}

func splitDoubleDash(args []string) ([]string, []string) {
	for i, arg := range args {
		if arg == "--" {
			return args[:i], args[i+1:]
		}
	}
	return args, nil
}
