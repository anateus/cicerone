package tui

import (
	"context"
	"fmt"
	"io"
	"strings"
	"time"

	"charm.land/bubbles/v2/viewport"
	tea "charm.land/bubbletea/v2"
	"cicerone/internal/domain"
	"cicerone/internal/homebrew"
	"cicerone/internal/store"
)

const (
	changelogDebounce = 250 * time.Millisecond
	searchDebounce    = 120 * time.Millisecond
)

type DataSource interface {
	QueryFeed(context.Context, domain.FeedFilter) ([]domain.FeedGroup, error)
	Preferences(context.Context) (domain.FeedFilter, error)
	SetPreferences(context.Context, domain.FeedFilter) error
}

type SeenRecorder interface {
	MarkEventsSeen(context.Context, []domain.EventID) error
}

type ChangelogSource interface {
	LoadChangelog(context.Context, domain.PackageID, domain.EventID) ([]store.ChangelogSection, error)
}

type CachedChangelogSource interface {
	LoadCachedChangelog(context.Context, domain.PackageID, domain.EventID) ([]store.ChangelogSection, error)
}

type PagedChangelogSource interface {
	LoadReleasePage(context.Context, domain.PackageID, domain.EventID, int, int) (store.ChangelogPage, error)
}

type PackageInfoSource interface {
	LoadPackageInfo(context.Context, domain.PackageID) (homebrew.PackageInfo, error)
}

type CachedPackageInfoSource interface {
	LoadCachedPackageInfo(context.Context, domain.PackageID) (homebrew.PackageInfo, bool, error)
}

type READMESource interface {
	LoadREADME(context.Context, domain.PackageID, domain.EventID) (store.PackageDocument, error)
}

type CachedREADMESource interface {
	LoadCachedREADME(context.Context, domain.PackageID) (store.PackageDocument, bool, error)
}

type CachedRepositoryTagsSource interface {
	LoadCachedRepositoryTags(context.Context, domain.PackageID) (store.PackageRepositoryTags, bool, error)
}

type ActionRunner interface {
	RunAction(context.Context, homebrew.Action, io.Writer) error
}

type InstalledRefresher interface {
	RefreshInstalled(context.Context) error
}

type Dependencies struct {
	Data        DataSource
	Changelog   ChangelogSource
	PackageInfo PackageInfoSource
	README      READMESource
	Tags        CachedRepositoryTagsSource
	Context     context.Context
	OnReady     tea.Cmd
	Actions     ActionRunner
	Installed   InstalledRefresher
	Send        func(tea.Msg)
}

type pane uint8

const (
	feedPane pane = iota
	inspectorPane
)

type Model struct {
	deps                                                            Dependencies
	width, height                                                   int
	groups                                                          []domain.FeedGroup
	selected                                                        int
	viewportOffset                                                  int
	focus                                                           pane
	expanded                                                        map[domain.EventID]bool
	filter                                                          domain.FeedFilter
	detailOpen                                                      bool
	loading, stale                                                  bool
	err                                                             error
	notification                                                    string
	light                                                           bool
	feedRequestID, changelogRequestID, detailRequestID, selectionID uint64
	notifyRequestID                                                 uint64
	changelog                                                       []store.ChangelogSection
	packageInfo                                                     homebrew.PackageInfo
	packageDescriptions                                             map[domain.PackageID]string
	descriptionRequests                                             map[domain.PackageID]bool
	sessionNew                                                      map[domain.EventID]bool
	seenBoundaryIndex                                               int
	readme                                                          store.PackageDocument
	repositoryTags                                                  []string
	repositoryTagsExpanded                                          bool
	packageInfoErr, readmeErr                                       error
	repositoryTagsErr                                               error
	changelogErr                                                    error
	changelogLoading                                                bool
	changelogArchiveStarted                                         bool
	changelogNextPage                                               int
	changelogMoreLoading                                            bool
	changelogMoreErr                                                error
	changelogPageCancel                                             context.CancelFunc
	detailProgress                                                  DetailProgress
	document                                                        store.DocumentKind
	documentExplicit                                                bool
	feedViewport, inspectorViewport                                 viewport.Model
	refreshAnchors                                                  map[uint64]domain.Anchor
	detailCancel                                                    context.CancelFunc
	ready                                                           bool
	pendingAction                                                   *homebrew.Action
	actionResult                                                    *homebrew.Action
	actionRunning                                                   bool
	actionOutput                                                    string
	actionAnchor                                                    domain.Anchor
	syncProgress                                                    map[string]SyncProgress
	activeSync                                                      map[string]bool
	searching                                                       bool
	searchQueryCancel                                               context.CancelFunc
}

func New(deps Dependencies) tea.Model { return NewModel(deps) }

func NewModel(deps Dependencies) Model {
	if deps.Context == nil {
		deps.Context = context.Background()
	}
	return Model{deps: deps, expanded: make(map[domain.EventID]bool), loading: true, feedRequestID: 1, document: store.DocumentChangelog,
		filter: domain.FeedFilter{
			Kinds: map[domain.EventKind]bool{}, Types: map[domain.PackageType]bool{domain.PackageFormula: true},
			Search: domain.SearchNames,
		},
		feedViewport: viewport.New(), inspectorViewport: viewport.New(), refreshAnchors: make(map[uint64]domain.Anchor),
		syncProgress: make(map[string]SyncProgress), activeSync: make(map[string]bool),
		packageDescriptions: make(map[domain.PackageID]string), descriptionRequests: make(map[domain.PackageID]bool),
		sessionNew: make(map[domain.EventID]bool), seenBoundaryIndex: -1}
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
		return m, tea.Batch(m.loadVisiblePackageDescriptions()...)
	case tea.WindowSizeMsg:
		m.width, m.height = msg.Width, msg.Height
		if m.width >= narrowBreakpoint {
			m.detailOpen = false
		}
		m.syncViewports()
		return m, tea.Batch(m.loadVisiblePackageDescriptions()...)
	case FeedLoaded:
		if msg.RequestID != m.feedRequestID {
			return m, nil
		}
		m.cancelSearchQuery()
		previousPackage := m.selectedEvent().PackageID
		anchor, captured := m.refreshAnchors[msg.RequestID]
		if !captured {
			anchor = m.anchor()
		}
		delete(m.refreshAnchors, msg.RequestID)
		m.loading, m.stale, m.err = false, false, msg.Err
		if msg.Err == nil {
			for _, group := range msg.Groups {
				for _, event := range group.Events {
					if !event.Seen {
						m.sessionNew[event.ID] = true
					}
				}
			}
			m.groups = m.partitionSeenGroups(msg.Groups)
			m.seenBoundaryIndex = m.findSeenBoundary(m.groups)
			restored := domain.RestoreSelection(anchor, m.groups)
			m.selected, m.viewportOffset = restored.FallbackIndex, restored.ViewportOffset
			m.clampSelection()
			if m.selectedEvent().PackageID != previousPackage {
				m.resetDetails()
			}
			m.syncViewports()
		}
		cmds := []tea.Cmd{m.debounceChangelog(), m.loadCachedPackageInfo(m.selectionID, m.selectedEvent()),
			m.loadCachedREADME(m.selectionID, m.selectedEvent()), m.loadCachedChangelog(m.selectionID, m.selectedEvent()),
			m.loadCachedRepositoryTags(m.selectionID, m.selectedEvent()), m.markFeedSeen(msg.Groups)}
		cmds = append(cmds, m.loadVisiblePackageDescriptions()...)
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
	case SyncProgress:
		m.syncProgress[msg.Source] = msg
		m.activeSync[msg.Source] = true
		m.notification = fmt.Sprintf("%s · %d commits scanned · %d updates · %d batches", msg.Source, msg.Commits, msg.Events, msg.Batches)
	case SyncDone:
		delete(m.activeSync, msg.Source)
	case PreferencesLoaded:
		if msg.Err == nil {
			msg.Filter.Now = m.filter.Now
			if msg.Filter.Kinds == nil {
				msg.Filter.Kinds = map[domain.EventKind]bool{}
			}
			if msg.Filter.Types == nil {
				msg.Filter.Types = map[domain.PackageType]bool{}
			}
			if len(msg.Filter.Types) == 0 {
				msg.Filter.Types[domain.PackageFormula] = true
			}
			if !validSearchScope(msg.Filter.Search) {
				msg.Filter.Search = domain.SearchNames
			}
			m.filter = msg.Filter
			m.feedRequestID++
			return m, m.queryFeed(m.feedRequestID)
		}
	case eventsSeen:
		if msg.Err != nil {
			m.err = msg.Err
			m.notification = "Error: record seen updates: " + msg.Err.Error()
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
	case SearchDebounced:
		if msg.RequestID != m.feedRequestID {
			return m, nil
		}
		m.cancelSearchQuery()
		searchContext, cancel := context.WithCancel(m.deps.Context)
		m.searchQueryCancel = cancel
		return m, tea.Batch(m.queryFeedContext(searchContext, msg.RequestID), m.savePreferences())
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
		if m.detailCancel != nil {
			m.detailCancel()
		}
		detailContext, cancel := context.WithCancel(m.deps.Context)
		m.detailCancel = cancel
		m.changelogRequestID++
		m.changelogLoading = true
		e := m.selectedEvent()
		m.detailRequestID++
		return m, tea.Batch(
			m.loadChangelog(detailContext, m.changelogRequestID, msg.SelectionID, e),
			m.loadPackageInfo(detailContext, m.detailRequestID, msg.SelectionID, e),
			m.loadREADME(detailContext, m.detailRequestID, msg.SelectionID, e),
		)
	case ChangelogLoaded:
		e := m.selectedEvent()
		if e.ID != msg.EventID || e.PackageID != msg.PackageID ||
			(msg.RequestID != 0 && (msg.RequestID != m.changelogRequestID || msg.SelectionID != m.selectionID)) {
			return m, nil
		}
		m.changelogLoading, m.changelogErr = false, msg.Err
		if m.changelogArchiveStarted {
			m.changelog = mergeChangelogSections(msg.Sections, m.changelog)
		} else {
			m.changelog = msg.Sections
		}
		if len(msg.Sections) == 0 && m.readme.ID != "" && !m.documentExplicit {
			m.document = store.DocumentREADME
		}
		if msg.Err == nil && !m.changelogArchiveStarted && githubReleaseSections(msg.Sections) {
			if _, ok := m.deps.Changelog.(PagedChangelogSource); ok {
				m.changelogArchiveStarted = true
				m.changelogNextPage = 1
				m.syncViewports()
				return m.beginReleasePage(1)
			}
		}
		m.syncViewports()
	case ChangelogPageLoaded:
		e := m.selectedEvent()
		if e.ID != msg.EventID || e.PackageID != msg.PackageID || msg.SelectionID != m.selectionID {
			return m, nil
		}
		if m.changelogPageCancel != nil {
			m.changelogPageCancel()
			m.changelogPageCancel = nil
		}
		m.changelogMoreLoading = false
		if msg.Err != nil {
			m.changelogMoreErr = msg.Err
			m.changelogNextPage = msg.Page
			m.syncViewports()
			return m, nil
		}
		m.changelogMoreErr = nil
		sections := msg.Result.Sections
		if msg.Page == 1 {
			sections = releasesAfterCurrent(m.changelog, sections)
		}
		m.changelog = mergeChangelogSections(m.changelog, sections)
		m.changelogNextPage = msg.Result.NextPage
		m.syncViewports()
	case PackageInfoLoaded:
		if msg.Err == nil && msg.Info.Description != "" {
			m.packageDescriptions[msg.PackageID] = msg.Info.Description
			m.syncViewports()
		}
		e := m.selectedEvent()
		if e.PackageID != msg.PackageID ||
			(msg.RequestID != 0 && (msg.RequestID != m.detailRequestID || msg.SelectionID != m.selectionID)) {
			return m, nil
		}
		if msg.Err == nil {
			m.packageInfo = msg.Info
		}
		m.packageInfoErr = msg.Err
	case READMELoaded:
		e := m.selectedEvent()
		if e.PackageID != msg.PackageID ||
			(msg.RequestID != 0 && (msg.RequestID != m.detailRequestID || msg.SelectionID != m.selectionID)) {
			return m, nil
		}
		if msg.Err == nil {
			m.readme = msg.Document
			if len(m.changelog) == 0 && msg.Document.ID != "" && !m.documentExplicit {
				m.document = store.DocumentREADME
			}
		}
		m.readmeErr = msg.Err
	case RepositoryTagsLoaded:
		if m.selectedEvent().PackageID != msg.PackageID ||
			(msg.SelectionID != 0 && msg.SelectionID != m.selectionID) {
			return m, nil
		}
		if msg.Err == nil {
			m.repositoryTags = append([]string(nil), msg.Record.Tags...)
		}
		m.repositoryTagsErr = msg.Err
	case DetailProgress:
		if msg.Sequence != 0 && msg.Sequence < m.detailProgress.Sequence {
			return m, nil
		}
		m.detailProgress = msg
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
	case tea.MouseClickMsg:
		return m.handleMouseClick(msg)
	case tea.MouseWheelMsg:
		return m.handleMouseWheel(msg)
	}
	return m, nil
}

func (m Model) View() tea.View {
	view := tea.NewView(m.render())
	view.AltScreen = true
	view.MouseMode = tea.MouseModeCellMotion
	view.KeyboardEnhancements.ReportEventTypes = true
	view.BackgroundColor = m.palette().canvasBG
	return view
}

func (m Model) handleKey(key tea.KeyPressMsg) (tea.Model, tea.Cmd) {
	if m.searching {
		return m.handleSearchKey(key)
	}
	if key.String() == "q" {
		return m, tea.Quit
	}
	if key.String() == "esc" {
		if m.width >= narrowBreakpoint && m.focus == inspectorPane {
			m.focus = feedPane
			m.syncViewports()
			return m, nil
		}
		if m.width < narrowBreakpoint && m.detailOpen {
			m.detailOpen = false
			m.syncViewports()
			return m, nil
		}
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
	readingInspector := (m.width >= narrowBreakpoint && m.focus == inspectorPane) || (m.width < narrowBreakpoint && m.detailOpen)
	if readingInspector {
		m.syncViewports()
		switch key.String() {
		case "t":
			m.repositoryTagsExpanded = !m.repositoryTagsExpanded
			m.syncViewports()
			return m, nil
		case "j", "down":
			m.inspectorViewport.ScrollDown(1)
			return m, nil
		case "k", "up":
			m.inspectorViewport.ScrollUp(1)
			return m, nil
		case "h", "left":
			m.inspectorViewport.ScrollLeft(4)
			return m, nil
		case "l", "right":
			m.inspectorViewport.ScrollRight(4)
			return m, nil
		case "m":
			return m.requestMoreReleases()
		case "enter":
			if m.width >= narrowBreakpoint {
				m.focus = feedPane
			} else {
				m.detailOpen = false
			}
			m.syncViewports()
			return m, nil
		}
	}
	switch key.String() {
	case "t":
		m.repositoryTagsExpanded = !m.repositoryTagsExpanded
		m.syncViewports()
	case "/":
		m.searching = true
		m.focus = feedPane
		m.detailOpen = false
		if !validSearchScope(m.filter.Search) {
			m.filter.Search = domain.SearchNames
		}
		m.syncViewports()
		return m, nil
	case "j", "down":
		if m.selected+1 < len(m.groups) {
			m.selected++
			m.selectionID++
			m.resetDetails()
			m.keepSelectionVisible()
			commands := []tea.Cmd{m.debounceChangelog()}
			commands = append(commands, m.loadVisiblePackageDescriptions()...)
			return m, tea.Batch(commands...)
		}
	case "k", "up":
		if m.selected > 0 {
			m.selected--
			m.selectionID++
			m.resetDetails()
			m.keepSelectionVisible()
			commands := []tea.Cmd{m.debounceChangelog()}
			commands = append(commands, m.loadVisiblePackageDescriptions()...)
			return m, tea.Batch(commands...)
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
			m.inspectorViewport.SetYOffset(0)
			m.syncViewports()
		}
	case " ":
		if len(m.groups) > 0 {
			m.expanded[m.groups[m.selected].ID] = !m.expanded[m.groups[m.selected].ID]
		}
	case "1":
		m.setPackageScope(true, false)
		return m.filterChanged()
	case "2":
		m.setPackageScope(false, true)
		return m.filterChanged()
	case "3":
		m.setPackageScope(true, true)
		return m.filterChanged()
	case "a":
		return m.requestSelectedAction()
	case "[":
		m.document = store.DocumentREADME
		m.documentExplicit = true
		m.syncViewports()
	case "]":
		m.document = store.DocumentChangelog
		m.documentExplicit = true
		m.syncViewports()
	}
	return m, nil
}

func (m Model) handleSearchKey(key tea.KeyPressMsg) (tea.Model, tea.Cmd) {
	switch key.String() {
	case "esc":
		m.searching = false
		m.syncViewports()
		return m, nil
	case "enter":
		m.searching = false
		m.syncViewports()
		return m.filterChanged()
	case "tab":
		m.filter.Search = nextSearchScope(m.filter.Search)
		return m.searchChanged()
	case "backspace":
		runes := []rune(m.filter.Query)
		if len(runes) > 0 {
			m.filter.Query = string(runes[:len(runes)-1])
			return m.searchChanged()
		}
		return m, nil
	case "ctrl+u":
		if m.filter.Query != "" {
			m.filter.Query = ""
			return m.searchChanged()
		}
		return m, nil
	}
	text := key.Key().Text
	if text == "" {
		return m, nil
	}
	m.filter.Query += text
	return m.searchChanged()
}

func (m Model) searchChanged() (tea.Model, tea.Cmd) {
	m.cancelSearchQuery()
	m.feedRequestID++
	id := m.feedRequestID
	return m, tea.Tick(searchDebounce, func(time.Time) tea.Msg {
		return SearchDebounced{RequestID: id}
	})
}

func nextSearchScope(scope domain.SearchScope) domain.SearchScope {
	switch scope {
	case domain.SearchNames:
		return domain.SearchDescriptions
	case domain.SearchDescriptions:
		return domain.SearchChangelogs
	case domain.SearchChangelogs:
		return domain.SearchREADMEs
	default:
		return domain.SearchNames
	}
}

func validSearchScope(scope domain.SearchScope) bool {
	switch scope {
	case domain.SearchNames, domain.SearchDescriptions, domain.SearchChangelogs, domain.SearchREADMEs:
		return true
	default:
		return false
	}
}

func (m Model) requestSelectedAction() (tea.Model, tea.Cmd) {
	if len(m.groups) == 0 {
		return m, nil
	}
	e := m.selectedEvent()
	kind := homebrew.Install
	if e.Installed {
		kind = homebrew.Upgrade
	}
	action := homebrew.Action{Kind: kind, Package: e.PackageID, Type: e.Type}
	return m, func() tea.Msg { return ActionRequested{Action: action} }
}

func (m Model) selectFeedIndex(index int) (tea.Model, tea.Cmd) {
	if index < 0 || index >= len(m.groups) || index == m.selected {
		return m, nil
	}
	m.selected = index
	m.selectionID++
	m.resetDetails()
	m.keepSelectionVisible()
	commands := []tea.Cmd{m.loadCachedPackageInfo(m.selectionID, m.selectedEvent()),
		m.loadCachedREADME(m.selectionID, m.selectedEvent()), m.loadCachedChangelog(m.selectionID, m.selectedEvent()),
		m.loadCachedRepositoryTags(m.selectionID, m.selectedEvent()), m.debounceChangelog()}
	commands = append(commands, m.loadVisiblePackageDescriptions()...)
	return m, tea.Batch(commands...)
}

func (m *Model) setPackageScope(formulae, casks bool) {
	m.filter.Types = map[domain.PackageType]bool{
		domain.PackageFormula: formulae,
		domain.PackageCask:    casks,
	}
}

func (m Model) filterChanged() (tea.Model, tea.Cmd) {
	m.cancelSearchQuery()
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

func (m *Model) resetDetails() {
	if m.detailCancel != nil {
		m.detailCancel()
		m.detailCancel = nil
	}
	m.packageInfo = homebrew.PackageInfo{}
	m.readme = store.PackageDocument{}
	m.repositoryTags = nil
	m.repositoryTagsExpanded = false
	m.changelog = nil
	m.changelogErr = nil
	m.changelogLoading = false
	m.changelogArchiveStarted = false
	m.changelogNextPage = 0
	m.changelogMoreLoading = false
	m.changelogMoreErr = nil
	if m.changelogPageCancel != nil {
		m.changelogPageCancel()
		m.changelogPageCancel = nil
	}
	m.packageInfoErr = nil
	m.readmeErr = nil
	m.repositoryTagsErr = nil
	m.err = nil
	m.documentExplicit = false
	m.inspectorViewport.SetYOffset(0)
}

func (m Model) queryFeed(id uint64) tea.Cmd {
	return m.queryFeedContext(m.deps.Context, id)
}

func (m Model) queryFeedContext(ctx context.Context, id uint64) tea.Cmd {
	return func() tea.Msg {
		if m.deps.Data == nil {
			return FeedLoaded{RequestID: id}
		}
		g, e := m.deps.Data.QueryFeed(ctx, m.filter)
		return FeedLoaded{RequestID: id, Groups: g, Err: e}
	}
}

func (m *Model) cancelSearchQuery() {
	if m.searchQueryCancel != nil {
		m.searchQueryCancel()
		m.searchQueryCancel = nil
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

func (m Model) markFeedSeen(groups []domain.FeedGroup) tea.Cmd {
	recorder, ok := m.deps.Data.(SeenRecorder)
	if !ok {
		return nil
	}
	ids := make([]domain.EventID, 0)
	for _, group := range groups {
		for _, event := range group.Events {
			ids = append(ids, event.ID)
		}
	}
	if len(ids) == 0 {
		return nil
	}
	return func() tea.Msg {
		return eventsSeen{Err: recorder.MarkEventsSeen(m.deps.Context, ids)}
	}
}

func (m Model) partitionSeenGroups(groups []domain.FeedGroup) []domain.FeedGroup {
	partitioned := make([]domain.FeedGroup, 0, len(groups))
	for _, group := range groups {
		if !m.groupPreviouslySeen(group) {
			partitioned = append(partitioned, group)
		}
	}
	for _, group := range groups {
		if m.groupPreviouslySeen(group) {
			partitioned = append(partitioned, group)
		}
	}
	return partitioned
}

func (m Model) groupPreviouslySeen(group domain.FeedGroup) bool {
	if len(group.Events) == 0 {
		return false
	}
	for _, event := range group.Events {
		if !event.Seen || m.sessionNew[event.ID] {
			return false
		}
	}
	return true
}

func (m Model) seenBoundary() int {
	if m.seenBoundaryIndex >= 0 {
		return min(m.seenBoundaryIndex, len(m.groups))
	}
	return m.findSeenBoundary(m.groups)
}

func (m Model) hasSeenSeparator() bool {
	boundary := m.seenBoundary()
	return boundary > 0 && boundary < len(m.groups)
}

func (m Model) findSeenBoundary(groups []domain.FeedGroup) int {
	for index, group := range groups {
		if m.groupPreviouslySeen(group) {
			return index
		}
	}
	return len(groups)
}
func (m Model) debounceChangelog() tea.Cmd {
	id := m.selectionID
	return tea.Tick(changelogDebounce, func(time.Time) tea.Msg { return ChangelogDebounced{SelectionID: id} })
}
func (m Model) loadChangelog(ctx context.Context, request, selection uint64, e domain.UpdateEvent) tea.Cmd {
	return func() tea.Msg {
		var s []store.ChangelogSection
		var err error
		if m.deps.Changelog != nil {
			s, err = m.deps.Changelog.LoadChangelog(ctx, e.PackageID, e.ID)
		}
		return ChangelogLoaded{RequestID: request, SelectionID: selection, EventID: e.ID, PackageID: e.PackageID, Sections: s, Err: err}
	}
}

func (m Model) loadReleasePage(ctx context.Context, selection uint64, e domain.UpdateEvent, page int) tea.Cmd {
	return func() tea.Msg {
		source, ok := m.deps.Changelog.(PagedChangelogSource)
		if !ok {
			return ChangelogPageLoaded{
				SelectionID: selection, EventID: e.ID, PackageID: e.PackageID, Page: page,
				Err: fmt.Errorf("release archive is unavailable"),
			}
		}
		result, err := source.LoadReleasePage(ctx, e.PackageID, e.ID, page, 10)
		return ChangelogPageLoaded{
			SelectionID: selection, EventID: e.ID, PackageID: e.PackageID, Page: page, Result: result, Err: err,
		}
	}
}

func (m Model) requestMoreReleases() (tea.Model, tea.Cmd) {
	if m.document != store.DocumentChangelog || m.changelogNextPage == 0 || m.changelogMoreLoading || len(m.groups) == 0 {
		return m, nil
	}
	return m.beginReleasePage(m.changelogNextPage)
}

func (m Model) beginReleasePage(page int) (tea.Model, tea.Cmd) {
	if m.changelogPageCancel != nil {
		m.changelogPageCancel()
	}
	ctx, cancel := context.WithCancel(m.deps.Context)
	m.changelogPageCancel = cancel
	m.changelogMoreLoading = true
	m.changelogMoreErr = nil
	return m, m.loadReleasePage(ctx, m.selectionID, m.selectedEvent(), page)
}

func githubReleaseSections(sections []store.ChangelogSection) bool {
	for _, section := range sections {
		source := strings.ToLower(section.SourceURL)
		if strings.Contains(source, "github.com/") && strings.Contains(source, "/releases/") {
			return true
		}
	}
	return false
}

func mergeChangelogSections(groups ...[]store.ChangelogSection) []store.ChangelogSection {
	seen := make(map[string]bool)
	var result []store.ChangelogSection
	for _, sections := range groups {
		for _, section := range sections {
			key := section.SourceURL
			if key == "" {
				key = section.ArtifactID + "\x00" + section.Version + "\x00" + section.Body
			}
			if seen[key] {
				continue
			}
			seen[key] = true
			result = append(result, section)
		}
	}
	return result
}

func releasesAfterCurrent(current, page []store.ChangelogSection) []store.ChangelogSection {
	currentURLs := make(map[string]bool)
	for _, section := range current {
		if section.SourceURL != "" {
			currentURLs[section.SourceURL] = true
		}
	}
	for index, section := range page {
		if currentURLs[section.SourceURL] {
			return page[index+1:]
		}
	}
	return page
}

func (m Model) loadPackageInfo(ctx context.Context, request, selection uint64, e domain.UpdateEvent) tea.Cmd {
	return func() tea.Msg {
		var info homebrew.PackageInfo
		var err error
		if m.deps.PackageInfo != nil {
			info, err = m.deps.PackageInfo.LoadPackageInfo(ctx, e.PackageID)
		}
		return PackageInfoLoaded{RequestID: request, SelectionID: selection, PackageID: e.PackageID, Info: info, Err: err}
	}
}

func (m Model) loadCachedPackageInfo(selection uint64, e domain.UpdateEvent) tea.Cmd {
	return func() tea.Msg {
		if e.PackageID == "" {
			return nil
		}
		source, ok := m.deps.PackageInfo.(CachedPackageInfoSource)
		if !ok {
			return nil
		}
		info, found, err := source.LoadCachedPackageInfo(m.deps.Context, e.PackageID)
		if !found && err == nil {
			return nil
		}
		return PackageInfoLoaded{SelectionID: selection, PackageID: e.PackageID, Info: info, Err: err}
	}
}

func (m *Model) loadVisiblePackageDescriptions() []tea.Cmd {
	if _, ok := m.deps.PackageInfo.(CachedPackageInfoSource); !ok || len(m.groups) == 0 {
		return nil
	}
	top := m.viewportOffset
	bottom := top + max(1, m.feedViewport.Height())
	selected := m.selectedEvent().PackageID
	line := 0
	boundary := m.seenBoundary()
	var commands []tea.Cmd
	for index, group := range m.groups {
		if m.hasSeenSeparator() && index == boundary {
			line++
		}
		height := feedRowHeight(m.feedViewport.Width())
		if m.expanded[group.ID] && len(group.Events) > 1 {
			height += len(group.Events) - 1
		}
		if line < bottom && line+height > top {
			event := group.Events[0]
			if !m.descriptionRequests[event.PackageID] {
				m.descriptionRequests[event.PackageID] = true
				if event.PackageID != selected {
					commands = append(commands, m.loadCachedPackageInfo(0, event))
				}
			}
		}
		line += height
		if line >= bottom {
			break
		}
	}
	return commands
}

func (m Model) loadCachedREADME(selection uint64, e domain.UpdateEvent) tea.Cmd {
	return func() tea.Msg {
		if e.PackageID == "" {
			return nil
		}
		source, ok := m.deps.README.(CachedREADMESource)
		if !ok {
			return nil
		}
		document, found, err := source.LoadCachedREADME(m.deps.Context, e.PackageID)
		if !found && err == nil {
			return nil
		}
		return READMELoaded{SelectionID: selection, PackageID: e.PackageID, Document: document, Err: err}
	}
}

func (m Model) loadCachedRepositoryTags(selection uint64, e domain.UpdateEvent) tea.Cmd {
	return func() tea.Msg {
		if e.PackageID == "" || m.deps.Tags == nil {
			return nil
		}
		record, found, err := m.deps.Tags.LoadCachedRepositoryTags(m.deps.Context, e.PackageID)
		if !found && err == nil {
			return nil
		}
		return RepositoryTagsLoaded{SelectionID: selection, PackageID: e.PackageID, Record: record, Err: err}
	}
}

func (m Model) loadCachedChangelog(selection uint64, e domain.UpdateEvent) tea.Cmd {
	return func() tea.Msg {
		if e.PackageID == "" {
			return nil
		}
		source, ok := m.deps.Changelog.(CachedChangelogSource)
		if !ok {
			return nil
		}
		sections, err := source.LoadCachedChangelog(m.deps.Context, e.PackageID, e.ID)
		if len(sections) == 0 && err == nil {
			return nil
		}
		return ChangelogLoaded{SelectionID: selection, EventID: e.ID, PackageID: e.PackageID, Sections: sections, Err: err}
	}
}

func (m Model) loadREADME(ctx context.Context, request, selection uint64, e domain.UpdateEvent) tea.Cmd {
	return func() tea.Msg {
		var document store.PackageDocument
		var err error
		if m.deps.README != nil {
			document, err = m.deps.README.LoadREADME(ctx, e.PackageID, e.ID)
		}
		return READMELoaded{RequestID: request, SelectionID: selection, PackageID: e.PackageID, Document: document, Err: err}
	}
}
