package cli

import (
	"errors"
	"flag"
	"fmt"
	"path/filepath"
	"strings"

	"github.com/iamcc30/codexm/internal/codex"
	"github.com/iamcc30/codexm/internal/config"
	"github.com/iamcc30/codexm/internal/sharedmcp"
)

func (a *App) cmdMCP(args []string) int {
	if len(args) == 0 || args[0] == "help" || args[0] == "--help" || args[0] == "-h" {
		a.printMCPHelp()
		return 0
	}
	subcommand, rest := args[0], args[1:]
	switch subcommand {
	case "path":
		if len(rest) != 0 {
			return a.fail(errors.New("usage: codexm mcp path"))
		}
		path, err := config.SharedMCPConfigPath()
		if err != nil {
			return a.fail(err)
		}
		fmt.Fprintln(a.Out, path)
		return 0
	case "sync":
		return a.cmdMCPSync(rest)
	case "exclude", "include":
		return a.cmdMCPInclusion(subcommand, rest)
	case "login", "logout":
		return a.cmdMCPAuth(subcommand, rest)
	case "add", "remove", "list", "get":
		return a.cmdMCPSharedCodex(subcommand, rest)
	default:
		fmt.Fprintf(a.Err, "unknown mcp command %q\n\n", subcommand)
		a.printMCPHelp()
		return 2
	}
}

func (a *App) printMCPHelp() {
	fmt.Fprint(a.Out, `Manage MCP servers shared across codexm profiles.

Usage:
  codexm mcp add [CODEX_MCP_ADD_ARGS...]
  codexm mcp remove NAME
  codexm mcp list
  codexm mcp get NAME
  codexm mcp sync [PROFILE|--all]
  codexm mcp exclude PROFILE SERVER
  codexm mcp include PROFILE SERVER
  codexm mcp login PROFILE NAME [CODEX_MCP_LOGIN_ARGS...]
  codexm mcp logout PROFILE NAME
  codexm mcp path

Examples:
  codexm mcp add context7 -- npx -y @upstash/context7-mcp
  codexm mcp add docs --url https://example.com/mcp --bearer-token-env-var DOCS_TOKEN
  codexm mcp exclude personal production-db
  codexm mcp login work github --scopes repo
`)
}

func (a *App) cmdMCPSharedCodex(subcommand string, args []string) int {
	home, err := config.SharedCodexHome()
	if err != nil {
		return a.fail(err)
	}
	if err := sharedmcp.EnsureSharedHome(home); err != nil {
		return a.fail(err)
	}
	runner, err := codex.Find()
	if err != nil {
		return a.fail(err)
	}
	code := a.runAndCode(runner, home, "", append([]string{"mcp", subcommand}, args...))
	if code != 0 || subcommand != "add" && subcommand != "remove" {
		return code
	}
	cfg, err := config.Load()
	if err != nil {
		return a.fail(err)
	}
	changed, err := a.syncMCPAll(cfg, true)
	if err != nil {
		return a.fail(err)
	}
	fmt.Fprintf(a.Out, "codexm: updated shared MCP configuration in %d profile(s)\n", changed)
	return 0
}

func (a *App) cmdMCPSync(args []string) int {
	fs := flag.NewFlagSet("mcp sync", flag.ContinueOnError)
	fs.SetOutput(a.Err)
	all := fs.Bool("all", false, "synchronize all profiles")
	if err := fs.Parse(args); err != nil {
		return 2
	}
	if fs.NArg() > 1 || *all && fs.NArg() != 0 {
		fmt.Fprintln(a.Err, "usage: codexm mcp sync [PROFILE|--all]")
		return 2
	}
	cfg, err := config.Load()
	if err != nil {
		return a.fail(err)
	}
	if *all || fs.NArg() == 0 {
		changed, err := a.syncMCPAll(cfg, true)
		if err != nil {
			return a.fail(err)
		}
		fmt.Fprintf(a.Out, "Synchronized %d profile(s); %d changed.\n", len(cfg.Profiles), changed)
		return 0
	}
	name := fs.Arg(0)
	p, ok := cfg.Profiles[name]
	if !ok {
		return a.fail(fmt.Errorf("profile %q does not exist", name))
	}
	result, err := a.syncMCPProfile(p, true)
	if err != nil {
		return a.fail(err)
	}
	fmt.Fprintf(a.Out, "Synchronized %s: inherited=%d local-overrides=%d excluded=%d changed=%t\n", name, len(result.Inherited), len(result.LocalOverrides), len(result.Excluded), result.Changed)
	return 0
}

func (a *App) cmdMCPInclusion(action string, args []string) int {
	if len(args) != 2 {
		fmt.Fprintf(a.Err, "usage: codexm mcp %s PROFILE SERVER\n", action)
		return 2
	}
	name, server := args[0], args[1]
	if action == "exclude" {
		path, err := config.SharedMCPConfigPath()
		if err != nil {
			return a.fail(err)
		}
		names, err := sharedmcp.ServerNames(path)
		if err != nil {
			return a.fail(err)
		}
		found := false
		for _, candidate := range names {
			if candidate == server {
				found = true
				break
			}
		}
		if !found {
			return a.fail(fmt.Errorf("shared MCP server %q does not exist", server))
		}
	}
	if err := config.Update(func(cfg *config.Config) error {
		p, ok := cfg.Profiles[name]
		if !ok {
			return fmt.Errorf("profile %q does not exist", name)
		}
		p = config.SetMCPExcluded(p, server, action == "exclude")
		if _, err := a.syncMCPProfile(p, true); err != nil {
			return err
		}
		cfg.Profiles[name] = p
		return nil
	}); err != nil {
		return a.fail(err)
	}
	verb := "Excluded"
	if action == "include" {
		verb = "Included"
	}
	fmt.Fprintf(a.Out, "%s shared MCP server %q for profile %q\n", verb, server, name)
	return 0
}

func (a *App) cmdMCPAuth(subcommand string, args []string) int {
	if len(args) < 2 {
		fmt.Fprintf(a.Err, "usage: codexm mcp %s PROFILE NAME [CODEX_MCP_%s_ARGS...]\n", subcommand, strings.ToUpper(subcommand))
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
	if _, err := a.syncMCPProfile(p, true); err != nil {
		return a.fail(err)
	}
	runner, err := codex.Find()
	if err != nil {
		return a.fail(err)
	}
	return a.runAndCode(runner, p.CodexHome, "", append([]string{"mcp", subcommand}, args[1:]...))
}

func (a *App) syncMCPProfile(profile config.Profile, write bool) (sharedmcp.Result, error) {
	if write {
		if err := codex.EnsureMCPOAuthCredentialStore(profile.CodexHome); err != nil {
			return sharedmcp.Result{}, err
		}
	}
	sharedPath, err := config.SharedMCPConfigPath()
	if err != nil {
		return sharedmcp.Result{}, err
	}
	return sharedmcp.Sync(sharedPath, filepath.Join(profile.CodexHome, "config.toml"), profile.ExcludedMCPServers, write)
}

func (a *App) syncMCPAll(cfg *config.Config, write bool) (int, error) {
	changed := 0
	for _, name := range config.SortedProfileNames(cfg) {
		result, err := a.syncMCPProfile(cfg.Profiles[name], write)
		if err != nil {
			return changed, fmt.Errorf("profile %s: %w", name, err)
		}
		if result.Changed {
			changed++
		}
	}
	return changed, nil
}
