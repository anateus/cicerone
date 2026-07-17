package tui

import (
	"context"
	"io"
	"time"

	"charm.land/bubbles/v2/viewport"
	tea "charm.land/bubbletea/v2"
	"cicerone/internal/domain"
	"cicerone/internal/homebrew"
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

type ActionRunner interface {
	RunAction(context.Context, homebrew.Action, io.Writer) error
}

type InstalledRefresher interface {
	RefreshInstalled(context.Context) error
}

type Dependencies struct {
	Data      DataSource
	Changelog ChangelogSource
	Context   context.Context
	OnReady   tea.Cmd
	Actions   ActionRunner
	Installed InstalledRefresher
	Send      func(tea.Msg)
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
	notifyRequestID                                uint64
	changelog                                      []store.ChangelogSection
	feedViewport, inspectorViewport                viewport.Model
	refreshAnchors                                 map[uint64]domain.Anchor
	ready                                          bool
	pendingAction                                  *homebrew.Action
	actionResult                                   *homebrew.Action
	actionRunning                                  bool
	actionOutput                                   string
	actionAnchor                                   domain.Anchor
}

func New(deps Dependencies) tea.Model { return NewModel(deps) }

func NewModel(deps Dependencies) Model {
	if deps.Context == nil {
		deps.Context = context.Background()
	}
	return Model{deps: deps, expanded: make(map[domain.EventID]bool), loading: true, feedRequestID: 1,
		filter:       domain.FeedFilter{Kinds: map[domain.EventKind]bool{}, Types: map[domain.PackageType]bool{}},
		feedViewport: viewport.New(), inspectorViewport: viewport.New(), refreshAnchors: make(map[uint64]domain.Anchor)}
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
		m.syncViewports()
	case tea.WindowSizeMsg:
		m.width, m.height = msg.Width, msg.Height
		if m.width >= narrowBreakpoint {
			m.detailOpen = false
		}
		m.syncViewports()
	case FeedLoaded:
		if msg.RequestID != m.feedRequestID {
			return m, nil
		}
		anchor, captured := m.refreshAnchors[msg.RequestID]
		if !captured {
			anchor = m.anchor()
		}
		delete(m.refreshAnchors, msg.RequestID)
		m.loading, m.stale, m.err = false, false, msg.Err
		if msg.Err == nil {
			m.groups = msg.Groups
			restored := domain.RestoreSelection(anchor, m.groups)
			m.selected, m.viewportOffset = restored.FallbackIndex, restored.ViewportOffset
			m.clampSelection()
			m.syncViewports()
		}
		cmds := []tea.Cmd{m.debounceChangelog()}
		if !m.ready {
			m.ready = true
			if m.deps.OnReady != nil {
				cmds = append(cmds, m.deps.OnReady)
			}
		}
		return m, tea.Batch(cmds...)
	case DatasetChanged:
		m.stale, m.loading = true, true
		m.feedRequestID++
		m.refreshAnchors[m.feedRequestID] = m.anchor()
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
			m.syncViewports()
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
		if msg.RequestID != 0 && msg.RequestID < m.notifyRequestID {
			return m, nil
		}
		if msg.SelectionID != 0 && msg.SelectionID != m.selectionID {
			return m, nil
		}
		if msg.RequestID > m.notifyRequestID {
			m.notifyRequestID = msg.RequestID
		}
		m.notification = msg.Text
		if msg.Err != nil {
			m.err = msg.Err
		}
	case SetLightMode:
		m.light = msg.Light
		m.syncViewports()
	case ActionRequested:
		if m.actionRunning || m.pendingAction != nil || m.actionResult != nil {
			return m, nil
		}
		action := msg.Action
		m.pendingAction = &action
		m.actionAnchor = m.anchor()
	case ActionConfirmed:
		if m.pendingAction == nil || m.actionRunning {
			return m, nil
		}
		action := *m.pendingAction
		m.pendingAction, m.actionRunning, m.actionOutput = nil, true, ""
		return m, m.runAction(action)
	case ActionOutput:
		if m.actionRunning {
			m.actionOutput = msg.Output
		}
	case ActionFinished:
		if !m.actionRunning {
			return m, nil
		}
		m.actionRunning, m.actionOutput = false, msg.Output
		if msg.Err != nil {
			m.err, m.actionResult = msg.Err, &msg.Action
			m.notification = "Error: " + msg.Err.Error()
			return m, nil
		}
		m.actionOutput, m.actionResult = "", nil
		return m, m.refreshInstalled()
	case installedRefreshed:
		if msg.Err != nil {
			m.err = msg.Err
			m.notification = "Error: " + msg.Err.Error()
			return m, nil
		}
		m.stale, m.loading = true, true
		m.feedRequestID++
		m.refreshAnchors[m.feedRequestID] = m.actionAnchor
		return m, m.queryFeed(m.feedRequestID)
	case tea.KeyPressMsg:
		return m.handleKey(msg)
	}
	return m, nil
}

func (m Model) View() tea.View { return tea.NewView(m.render()) }

func (m Model) handleKey(key tea.KeyPressMsg) (tea.Model, tea.Cmd) {
	switch key.String() {
	case "q", "esc":
		return m, tea.Quit
	}
	if m.pendingAction != nil {
		switch key.String() {
		case "y", "enter":
			return m.Update(ActionConfirmed{})
		case "n":
			m.pendingAction = nil
		}
		return m, nil
	}
	if m.actionResult != nil {
		return m, nil
	}
	if m.actionRunning {
		return m, nil
	}
	switch key.String() {
	case "j", "down":
		if m.selected+1 < len(m.groups) {
			m.selected++
			m.selectionID++
			m.keepSelectionVisible()
			return m, m.debounceChangelog()
		}
	case "k", "up":
		if m.selected > 0 {
			m.selected--
			m.selectionID++
			m.keepSelectionVisible()
			return m, m.debounceChangelog()
		}
	case "h", "left":
		if m.width >= 100 {
			m.focus = feedPane
		} else {
			m.detailOpen = false
		}
	case "l", "right":
		if m.width >= 100 {
			m.focus = inspectorPane
		} else if len(m.groups) > 0 {
			m.detailOpen = true
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
	case " ":
		if len(m.groups) > 0 {
			m.expanded[m.groups[m.selected].ID] = !m.expanded[m.groups[m.selected].ID]
		}
	case "a":
		if len(m.groups) > 0 {
			e := m.selectedEvent()
			kind := homebrew.Install
			if e.Installed {
				kind = homebrew.Upgrade
			}
			action := homebrew.Action{Kind: kind, Package: e.PackageID, Type: e.Type}
			return m, func() tea.Msg { return ActionRequested{Action: action} }
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
