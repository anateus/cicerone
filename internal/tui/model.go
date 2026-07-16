package tui

import (
	"context"
	"time"

	"charm.land/bubbles/v2/viewport"
	tea "charm.land/bubbletea/v2"
	"cicerone/internal/domain"
	"cicerone/internal/store"
)

const changelogDebounce = 250 * time.Millisecond

type DataSource interface {
	QueryFeed(context.Context, domain.FeedFilter) ([]domain.FeedGroup, error)
	Preferences(context.Context) (domain.FeedFilter, error)
	SetPreferences(context.Context, domain.FeedFilter) error
}

type ChangelogSource interface {
	LoadChangelog(context.Context, domain.PackageID, domain.EventID) ([]store.ChangelogSection, error)
}

type Dependencies struct {
	Data      DataSource
	Changelog ChangelogSource
	Context   context.Context
}

type pane uint8

const (
	feedPane pane = iota
	inspectorPane
)

type Model struct {
	deps                                           Dependencies
	width, height                                  int
	groups                                         []domain.FeedGroup
	selected                                       int
	viewportOffset                                 int
	focus                                          pane
	expanded                                       map[domain.EventID]bool
	filter                                         domain.FeedFilter
	detailOpen                                     bool
	loading, stale                                 bool
	err                                            error
	notification                                   string
	light                                          bool
	feedRequestID, changelogRequestID, selectionID uint64
	changelog                                      []store.ChangelogSection
	feedViewport, inspectorViewport                viewport.Model
}

func New(deps Dependencies) tea.Model { return NewModel(deps) }

func NewModel(deps Dependencies) Model {
	if deps.Context == nil {
		deps.Context = context.Background()
	}
	return Model{deps: deps, expanded: make(map[domain.EventID]bool), loading: true, feedRequestID: 1,
		filter:       domain.FeedFilter{Kinds: map[domain.EventKind]bool{}, Types: map[domain.PackageType]bool{}},
		feedViewport: viewport.New(), inspectorViewport: viewport.New()}
}

func (m Model) Init() tea.Cmd {
	cmds := []tea.Cmd{m.queryFeed(m.feedRequestID)}
	if m.deps.Data != nil {
		cmds = append(cmds, m.loadPreferences())
	}
	return tea.Batch(cmds...)
}

func (m Model) Update(msg tea.Msg) (tea.Model, tea.Cmd) {
	switch msg := msg.(type) {
	case WindowSize:
		m.width, m.height = msg.Width, msg.Height
		if m.width >= 100 {
			m.detailOpen = false
		}
	case tea.WindowSizeMsg:
		m.width, m.height = msg.Width, msg.Height
	case FeedLoaded:
		if msg.RequestID != m.feedRequestID {
			return m, nil
		}
		anchor := m.anchor()
		m.loading, m.stale, m.err = false, false, msg.Err
		if msg.Err == nil {
			m.groups = msg.Groups
			restored := domain.RestoreSelection(anchor, m.groups)
			m.selected, m.viewportOffset = restored.FallbackIndex, restored.ViewportOffset
			m.clampSelection()
		}
		return m, m.debounceChangelog()
	case DatasetChanged:
		m.stale, m.loading = true, true
		m.feedRequestID++
		return m, m.queryFeed(m.feedRequestID)
	case PreferencesLoaded:
		if msg.Err == nil {
			msg.Filter.Now = m.filter.Now
			if msg.Filter.Kinds == nil {
				msg.Filter.Kinds = map[domain.EventKind]bool{}
			}
			if msg.Filter.Types == nil {
				msg.Filter.Types = map[domain.PackageType]bool{}
			}
			m.filter = msg.Filter
			m.feedRequestID++
			return m, m.queryFeed(m.feedRequestID)
		}
	case ToggleFilter:
		m.filter.Kinds[msg.Kind] = !m.filter.Kinds[msg.Kind]
		return m.filterChanged()
	case ToggleTypeFilter:
		m.filter.Types[msg.Type] = !m.filter.Types[msg.Type]
		return m.filterChanged()
	case SearchChanged:
		m.filter.Query = msg.Text
		return m.filterChanged()
	case ToggleRollUp:
		m.filter.RollUp = !m.filter.RollUp
		return m.filterChanged()
	case ToggleExpanded:
		if len(m.groups) > 0 {
			m.expanded[m.groups[m.selected].ID] = !m.expanded[m.groups[m.selected].ID]
		}
	case ChangelogDebounced:
		if msg.SelectionID != m.selectionID || len(m.groups) == 0 {
			return m, nil
		}
		m.changelogRequestID++
		e := m.selectedEvent()
		return m, m.loadChangelog(m.changelogRequestID, msg.SelectionID, e)
	case ChangelogLoaded:
		e := m.selectedEvent()
		if msg.RequestID != m.changelogRequestID || msg.SelectionID != m.selectionID || e.ID != msg.EventID || e.PackageID != msg.PackageID {
			return m, nil
		}
		m.err, m.changelog = msg.Err, msg.Sections
	case Notify:
		if msg.SelectionID != 0 && msg.SelectionID != m.selectionID {
			return m, nil
		}
		m.notification = msg.Text
		if msg.Err != nil {
			m.err = msg.Err
		}
	case SetLightMode:
		m.light = msg.Light
	case tea.KeyPressMsg:
		return m.handleKey(msg)
	}
	return m, nil
}

func (m Model) View() tea.View { return tea.NewView(m.render()) }

func (m Model) handleKey(key tea.KeyPressMsg) (tea.Model, tea.Cmd) {
	switch key.String() {
	case "j", "down":
		if m.selected+1 < len(m.groups) {
			m.selected++
			m.selectionID++
			return m, m.debounceChangelog()
		}
	case "k", "up":
		if m.selected > 0 {
			m.selected--
			m.selectionID++
			return m, m.debounceChangelog()
		}
	case "tab":
		if m.width >= 100 {
			if m.focus == feedPane {
				m.focus = inspectorPane
			} else {
				m.focus = feedPane
			}
		}
	case "enter":
		if m.width < 100 && len(m.groups) > 0 {
			m.detailOpen = true
		}
		if m.width >= 100 {
			m.focus = inspectorPane
		}
	case "esc":
		m.detailOpen = false
		m.focus = feedPane
	case " ":
		if len(m.groups) > 0 {
			m.expanded[m.groups[m.selected].ID] = !m.expanded[m.groups[m.selected].ID]
		}
	}
	return m, nil
}

func (m Model) filterChanged() (tea.Model, tea.Cmd) {
	m.feedRequestID++
	return m, tea.Batch(m.queryFeed(m.feedRequestID), m.savePreferences())
}

func (m Model) anchor() domain.Anchor {
	if len(m.groups) == 0 {
		return domain.Anchor{FallbackIndex: m.selected, ViewportOffset: m.viewportOffset}
	}
	e := m.selectedEvent()
	return domain.Anchor{GroupID: m.groups[m.selected].ID, ChildEventID: e.ID, FallbackIndex: m.selected, ViewportOffset: m.viewportOffset}
}

func (m *Model) clampSelection() {
	if len(m.groups) == 0 {
		m.selected = 0
		return
	}
	if m.selected < 0 {
		m.selected = 0
	}
	if m.selected >= len(m.groups) {
		m.selected = len(m.groups) - 1
	}
}
func (m Model) selectedEvent() domain.UpdateEvent {
	if len(m.groups) == 0 {
		return domain.UpdateEvent{}
	}
	return m.groups[m.selected].Events[0]
}

func (m Model) queryFeed(id uint64) tea.Cmd {
	return func() tea.Msg {
		if m.deps.Data == nil {
			return FeedLoaded{RequestID: id}
		}
		g, e := m.deps.Data.QueryFeed(m.deps.Context, m.filter)
		return FeedLoaded{RequestID: id, Groups: g, Err: e}
	}
}
func (m Model) loadPreferences() tea.Cmd {
	return func() tea.Msg {
		f, e := m.deps.Data.Preferences(m.deps.Context)
		return PreferencesLoaded{Filter: f, Err: e}
	}
}
func (m Model) savePreferences() tea.Cmd {
	return func() tea.Msg {
		if m.deps.Data == nil {
			return preferencesSaved{}
		}
		return preferencesSaved{Err: m.deps.Data.SetPreferences(m.deps.Context, m.filter)}
	}
}
func (m Model) debounceChangelog() tea.Cmd {
	id := m.selectionID
	return tea.Tick(changelogDebounce, func(time.Time) tea.Msg { return ChangelogDebounced{SelectionID: id} })
}
func (m Model) loadChangelog(request, selection uint64, e domain.UpdateEvent) tea.Cmd {
	return func() tea.Msg {
		var s []store.ChangelogSection
		var err error
		if m.deps.Changelog != nil {
			s, err = m.deps.Changelog.LoadChangelog(m.deps.Context, e.PackageID, e.ID)
		}
		return ChangelogLoaded{RequestID: request, SelectionID: selection, EventID: e.ID, PackageID: e.PackageID, Sections: s, Err: err}
	}
}
