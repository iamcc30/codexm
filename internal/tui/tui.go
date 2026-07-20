package tui

import (
	"context"
	"fmt"
	"io"
	"os"
	"strings"
	"sync"
	"time"

	"github.com/charmbracelet/bubbles/textinput"
	tea "github.com/charmbracelet/bubbletea"
	"github.com/charmbracelet/lipgloss"
	"github.com/iamcc30/codexm/internal/appserver"
	"github.com/iamcc30/codexm/internal/monitor"
)

var (
	accentStyle lipgloss.Style
	dimStyle    lipgloss.Style
	okStyle     lipgloss.Style
	warnStyle   lipgloss.Style
	badStyle    lipgloss.Style
	stylesOnce  sync.Once
)

func ensureStyles() {
	stylesOnce.Do(func() {
		accentStyle = lipgloss.NewStyle().Foreground(lipgloss.Color("78")).Bold(true)
		dimStyle = lipgloss.NewStyle().Foreground(lipgloss.Color("245"))
		okStyle = lipgloss.NewStyle().Foreground(lipgloss.Color("78"))
		warnStyle = lipgloss.NewStyle().Foreground(lipgloss.Color("214"))
		badStyle = lipgloss.NewStyle().Foreground(lipgloss.Color("203"))
	})
}

type snapshotMsg monitor.Snapshot

type model struct {
	store     *monitor.Store
	updates   <-chan monitor.Snapshot
	snapshot  monitor.Snapshot
	width     int
	height    int
	tab       int
	cursor    int
	filter    textinput.Model
	filtering bool
	quitting  bool
}

func Run(ctx context.Context, store *monitor.Store, output io.Writer) error {
	ensureStyles()
	updates, unsubscribe := store.Subscribe()
	defer unsubscribe()
	input := textinput.New()
	input.Placeholder = "filter"
	m := model{store: store, updates: updates, snapshot: store.Snapshot(), tab: 0, filter: input}
	options := []tea.ProgramOption{tea.WithContext(ctx), tea.WithInput(os.Stdin)}
	if output != nil {
		options = append(options, tea.WithOutput(output))
	}
	_, err := tea.NewProgram(m, options...).Run()
	return err
}

func (m model) Init() tea.Cmd { return waitSnapshot(m.updates) }

func waitSnapshot(updates <-chan monitor.Snapshot) tea.Cmd {
	return func() tea.Msg {
		snapshot, ok := <-updates
		if !ok {
			return tea.Quit()
		}
		return snapshotMsg(snapshot)
	}
}

func (m model) Update(message tea.Msg) (tea.Model, tea.Cmd) {
	switch msg := message.(type) {
	case snapshotMsg:
		m.snapshot = monitor.Snapshot(msg)
		m.clampCursor()
		return m, waitSnapshot(m.updates)
	case tea.WindowSizeMsg:
		m.width, m.height = msg.Width, msg.Height
	case tea.KeyMsg:
		if m.filtering {
			switch msg.String() {
			case "esc", "enter":
				m.filtering = false
				m.filter.Blur()
				return m, nil
			}
			var cmd tea.Cmd
			m.filter, cmd = m.filter.Update(msg)
			m.clampCursor()
			return m, cmd
		}
		switch msg.String() {
		case "q", "ctrl+c":
			m.quitting = true
			return m, tea.Quit
		case "tab", "right", "l":
			m.tab = (m.tab + 1) % 6
			m.cursor = 0
		case "shift+tab", "left", "h":
			m.tab = (m.tab + 5) % 6
			m.cursor = 0
		case "up", "k":
			if m.cursor > 0 {
				m.cursor--
			}
		case "down", "j":
			m.cursor++
		case "/":
			m.filtering = true
			m.filter.Focus()
			return m, textinput.Blink
		default:
			if len(msg.String()) == 1 && msg.String()[0] >= '1' && msg.String()[0] <= '6' {
				m.tab = int(msg.String()[0] - '1')
				m.cursor = 0
			}
		}
	}
	m.clampCursor()
	return m, nil
}

func (m *model) clampCursor() {
	rows := m.selectableRows()
	if rows == 0 {
		m.cursor = 0
		return
	}
	if m.cursor >= rows {
		m.cursor = rows - 1
	}
}

func (m model) selectableRows() int {
	switch m.tab {
	case 1:
		return len(m.snapshot.Accounts)
	case 2:
		return len(m.snapshot.Projects)
	case 3:
		count := 0
		for _, item := range m.snapshot.Sessions {
			if m.matches(item.Title, item.Preview, item.Profile, item.Project, item.Model) {
				count++
			}
		}
		return count
	case 4:
		count := 0
		for _, item := range m.snapshot.Tasks {
			if m.matches(item.Title, item.Profile, item.Project, item.Status) {
				count++
			}
		}
		return count
	case 5:
		return len(m.snapshot.Subagents)
	default:
		return 0
	}
}

func (m model) View() string {
	ensureStyles()
	if m.quitting {
		return ""
	}
	zh := m.snapshot.Locale == "zh-CN"
	tabs := []string{"Overview", "Accounts", "Projects", "Sessions", "Tasks", "Subagents"}
	if zh {
		tabs = []string{"总览", "帐号", "项目", "Sessions", "任务", "子代理"}
	}
	var nav []string
	for i, tab := range tabs {
		label := fmt.Sprintf("%d %s", i+1, tab)
		if i == m.tab {
			label = accentStyle.Render("[" + label + "]")
		} else {
			label = dimStyle.Render(label)
		}
		nav = append(nav, label)
	}
	header := accentStyle.Render("codexm") + "  " + strings.Join(nav, "  ")
	if m.width > 0 && lipgloss.Width(header) > m.width {
		header = accentStyle.Render("codexm") + "  " + accentStyle.Render(tabs[m.tab])
	}
	body := m.renderTab(zh)
	footerText := "tab/1-6 navigate · j/k select · / filter · q quit · read-only"
	if zh {
		footerText = "tab/1-6 导航 · j/k 选择 · / 过滤 · q 退出 · 只读"
	}
	footer := dimStyle.Render(footerText + " · " + time.Now().Format("15:04:05"))
	if m.filtering || m.filter.Value() != "" {
		footer = m.filter.View() + "  " + footer
	}
	return header + "\n" + strings.Repeat("─", max(1, min(m.width, 120))) + "\n" + body + "\n" + footer
}

func (m model) renderTab(zh bool) string {
	switch m.tab {
	case 0:
		return m.renderOverview(zh)
	case 1:
		rows := make([]string, 0, len(m.snapshot.Accounts))
		for _, account := range m.snapshot.Accounts {
			status := badStyle.Render("error")
			if account.CodexHealthy {
				status = okStyle.Render("healthy")
			}
			if m.width < 70 {
				rows = append(rows, fmt.Sprintf("%s %s %s", account.Profile, account.Email, status))
			} else {
				usage := "—"
				if account.Lifetime != nil {
					usage = formatTokens(*account.Lifetime)
				}
				mcp := okStyle.Render("mcp:ok")
				if !account.MCPHealthy {
					mcp = badStyle.Render("mcp:error")
				}
				rows = append(rows, fmt.Sprintf("%-12s %-24s %-9s p:%-4s s:%-4s %-9s %s %s",
					account.Profile, account.Email, account.Plan, formatLimit(account.Primary),
					formatLimit(account.Secondary), usage, mcp, status))
			}
		}
		return m.list(rows)
	case 2:
		rows := make([]string, 0, len(m.snapshot.Projects))
		for _, project := range m.snapshot.Projects {
			mirror := fmt.Sprintf("mirror:%d", project.Mirror.Sessions+project.Mirror.ArchivedSessions)
			if len(project.Mirror.Pending.Conflicts) > 0 {
				mirror = badStyle.Render(fmt.Sprintf("conflicts:%d", len(project.Mirror.Pending.Conflicts)))
			}
			if m.width < 70 {
				rows = append(rows, fmt.Sprintf("%s %s %s", project.Profile, mirror, project.Root))
			} else {
				tokens := "—"
				if project.TokenSessions > 0 {
					tokens = fmt.Sprintf("%s/%d", formatTokens(project.Tokens), project.TokenSessions)
				}
				rows = append(rows, fmt.Sprintf("%-12s sessions:%-5d active:%-3d tokens:%-10s %-14s %-14s %s",
					project.Profile, project.Sessions, project.ActiveTasks, tokens, mirror,
					project.GitBranch, project.Root))
			}
		}
		return m.list(rows)
	case 3:
		rows := []string{}
		for _, item := range m.snapshot.Sessions {
			if !m.matches(item.Title, item.Preview, item.Profile, item.Project, item.Model) {
				continue
			}
			tokens := "—"
			if item.TokenKnown {
				tokens = formatTokens(item.Tokens)
			}
			if m.width < 70 {
				rows = append(rows, fmt.Sprintf("%s %s %s %s", item.Profile, renderStatus(item.Status), item.ID, item.Title))
			} else {
				archive := ""
				if item.Archived {
					archive = "archived"
				}
				rows = append(rows, fmt.Sprintf("%-12s %-18s %-9s %-8s %-14s %-10s %-8s %s %s",
					item.Profile, renderStatus(item.Status), tokens, formatAge(item.UpdatedAt),
					firstDisplay(item.Model, item.Source), item.GitBranch, archive, item.ID, item.Title))
			}
		}
		return m.list(rows)
	case 4:
		rows := []string{}
		for _, item := range m.snapshot.Tasks {
			if !m.matches(item.Title, item.Profile, item.Project, item.Status) {
				continue
			}
			if m.width < 70 {
				rows = append(rows, fmt.Sprintf("%s %s %s", item.Profile, renderStatus(item.Status), item.Title))
			} else {
				rows = append(rows, fmt.Sprintf("%-12s %-18s %-6s %s", item.Profile, renderStatus(item.Status), formatAge(item.LastActivity), item.Title))
			}
		}
		return m.list(rows)
	default:
		rows := []string{}
		for _, item := range m.snapshot.Subagents {
			tree := strings.Repeat("  ", min(item.Depth, 8)) + "↳"
			flag := ""
			if item.Cycle {
				flag = badStyle.Render(" cycle")
			} else if item.Orphan {
				flag = warnStyle.Render(" orphan")
			}
			label := firstDisplay(item.Nickname, item.ID)
			detail := firstDisplay(item.Role, item.Path, item.Project)
			rows = append(rows, fmt.Sprintf("%s %-12s %-16s %-18s parent:%-12s task:%-12s %s%s",
				tree, item.Profile, renderStatus(item.Status), label, item.ParentID,
				item.TaskID, detail, flag))
		}
		return m.list(rows)
	}
}

func (m model) renderOverview(zh bool) string {
	s := m.snapshot.Summary
	labels := []string{"Profiles", "Projects", "Sessions", "Active", "Approval", "Input", "Unmanaged", "Failures"}
	if zh {
		labels = []string{"帐号", "项目", "Sessions", "活动", "审批", "输入", "未托管", "异常"}
	}
	values := []int{s.Profiles, s.Projects, s.Sessions, s.ActiveTasks, s.WaitingApproval, s.WaitingInput, s.Unmanaged, s.ServiceFailures}
	columns := 4
	if m.width < 70 {
		columns = 2
	}
	var rows []string
	for i := 0; i < len(labels); i += columns {
		var cells []string
		for j := i; j < i+columns && j < len(labels); j++ {
			cells = append(cells, fmt.Sprintf("%-14s %5d", labels[j], values[j]))
		}
		rows = append(rows, strings.Join(cells, "  "))
	}
	if len(m.snapshot.Warnings) > 0 {
		rows = append(rows, "", warnStyle.Render(strings.Join(m.snapshot.Warnings, "\n")))
	}
	return strings.Join(rows, "\n")
}

func (m model) list(rows []string) string {
	if len(rows) == 0 {
		return dimStyle.Render("—")
	}
	available := m.height - 5
	if available < 3 {
		available = 3
	}
	if m.cursor >= len(rows) {
		m.cursor = len(rows) - 1
	}
	start := 0
	if m.cursor >= available {
		start = m.cursor - available + 1
	}
	end := min(len(rows), start+available)
	var output []string
	for i := start; i < end; i++ {
		line := truncate(rows[i], max(20, m.width))
		if i == m.cursor {
			line = accentStyle.Render("› " + line)
		} else {
			line = "  " + line
		}
		output = append(output, line)
	}
	return strings.Join(output, "\n")
}

func (m model) matches(values ...string) bool {
	query := strings.ToLower(strings.TrimSpace(m.filter.Value()))
	if query == "" {
		return true
	}
	return strings.Contains(strings.ToLower(strings.Join(values, " ")), query)
}

func renderStatus(status string) string {
	switch status {
	case "active", "idle", "healthy":
		return okStyle.Render(status)
	case "waiting_approval", "waiting_input", "unmanaged":
		return warnStyle.Render(status)
	case "error":
		return badStyle.Render(status)
	default:
		return dimStyle.Render(status)
	}
}

func formatTokens(value int64) string {
	switch {
	case value >= 1_000_000:
		return fmt.Sprintf("%.1fM", float64(value)/1_000_000)
	case value >= 1_000:
		return fmt.Sprintf("%.1fk", float64(value)/1_000)
	default:
		return fmt.Sprintf("%d", value)
	}
}

func formatLimit(window *appserver.RateLimitWindow) string {
	if window == nil {
		return "—"
	}
	return fmt.Sprintf("%d%%", window.UsedPercent)
}

func firstDisplay(values ...string) string {
	for _, value := range values {
		if value != "" {
			return value
		}
	}
	return "—"
}

func formatAge(value time.Time) string {
	if value.IsZero() {
		return "—"
	}
	duration := time.Since(value)
	if duration < time.Minute {
		return "now"
	}
	if duration < time.Hour {
		return fmt.Sprintf("%dm", int(duration.Minutes()))
	}
	if duration < 24*time.Hour {
		return fmt.Sprintf("%dh", int(duration.Hours()))
	}
	return fmt.Sprintf("%dd", int(duration.Hours()/24))
}

func truncate(value string, width int) string {
	runes := []rune(value)
	if len(runes) <= width {
		return value
	}
	if width <= 1 {
		return "…"
	}
	return string(runes[:width-1]) + "…"
}

func min(a, b int) int {
	if a < b {
		return a
	}
	return b
}

func max(a, b int) int {
	if a > b {
		return a
	}
	return b
}
