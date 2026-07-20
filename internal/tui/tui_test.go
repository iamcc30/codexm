package tui

import (
	"strings"
	"testing"

	"github.com/charmbracelet/bubbles/textinput"
	tea "github.com/charmbracelet/bubbletea"
	"github.com/iamcc30/codexm/internal/monitor"
)

func TestModelResizeNavigationAndNarrowRendering(t *testing.T) {
	input := newModelForTest()
	updated, _ := input.Update(tea.WindowSizeMsg{Width: 42, Height: 10})
	m := updated.(model)
	if view := m.View(); !strings.Contains(view, "codexm") || !strings.Contains(view, "Profiles") {
		t.Fatalf("narrow overview did not render: %q", view)
	}
	updated, _ = m.Update(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune{'4'}})
	m = updated.(model)
	if m.tab != 3 || !strings.Contains(m.View(), "session title") || !strings.Contains(m.View(), "session-id-one") {
		t.Fatalf("session navigation failed: tab=%d view=%q", m.tab, m.View())
	}
	updated, _ = m.Update(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune{'/'}})
	m = updated.(model)
	if !m.filtering {
		t.Fatal("filter mode did not activate")
	}
}

func TestModelRecoversWhenAHealthySnapshotFollowsFailure(t *testing.T) {
	m := newModelForTest()
	m.snapshot.Services = []monitor.Service{{Profile: "test", Error: "connection lost"}}
	updated, _ := m.Update(snapshotMsg(monitor.Snapshot{
		Locale: "en", Summary: monitor.Summary{Profiles: 1},
		Services: []monitor.Service{{Profile: "test", Healthy: true}},
	}))
	recovered := updated.(model)
	if len(recovered.snapshot.Services) != 1 || !recovered.snapshot.Services[0].Healthy {
		t.Fatalf("healthy snapshot did not replace disconnected state: %+v", recovered.snapshot.Services)
	}
	if !strings.Contains(recovered.View(), "Profiles") {
		t.Fatalf("recovered model did not render: %q", recovered.View())
	}
}

func TestModelPersistsCursorClamp(t *testing.T) {
	m := newModelForTest()
	m.tab = 3
	for range 4 {
		updated, _ := m.Update(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune{'j'}})
		m = updated.(model)
	}
	if m.cursor != 0 {
		t.Fatalf("cursor moved beyond the only visible row: %d", m.cursor)
	}
	m.snapshot.Sessions = append(m.snapshot.Sessions,
		monitor.Session{ID: "session-id-two", Title: "second session", Profile: "test"},
		monitor.Session{ID: "session-id-three", Title: "third session", Profile: "test"},
	)
	m.cursor = 2
	updated, _ := m.Update(snapshotMsg(monitor.Snapshot{
		Locale:   "en",
		Sessions: []monitor.Session{{ID: "session-id-one", Title: "session title", Profile: "test"}},
	}))
	m = updated.(model)
	if m.cursor != 0 {
		t.Fatalf("snapshot shrink did not persist cursor clamp: %d", m.cursor)
	}
}

func newModelForTest() model {
	input := textInputForTest()
	return model{
		snapshot: monitor.Snapshot{
			Locale:   "en",
			Summary:  monitor.Summary{Profiles: 1, Projects: 1, Sessions: 1},
			Sessions: []monitor.Session{{ID: "session-id-one", Title: "session title", Profile: "test"}},
		},
		width: 80, height: 20, filter: input,
	}
}

func textInputForTest() textinput.Model {
	input := textinput.New()
	input.Placeholder = "filter"
	return input
}
