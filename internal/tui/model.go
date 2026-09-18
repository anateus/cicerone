package tui

import (
	"context"
	"fmt"
	"io"
	"strings"
	"time"

	"charm.land/bubbles/v2/viewport"
	tea "charm.land/bubbletea/v2"
	"github.com/anateus/cicerone/internal/domain"
	"github.com/anateus/cicerone/internal/homebrew"
	"github.com/anateus/cicerone/internal/store"
)

const (
	changelogDebounce  = 250 * time.Millisecond
	searchDebounce     = 120 * time.Millisecond
	detailSpinInterval = 100 * time.Millisecond
)

var detailSpinnerFrames = [...]string{"⠋", "⠙", "⠹", "⠸", "⠼", "⠴", "⠦", "⠧", "⠇", "⠏"}

type DataSource interface {
	QueryFeed(context.Context, domain.FeedFilter) ([]domain.FeedGroup, error)
	Preferences(context.Context) (domain.FeedFilter, error)
	SetPreferences(context.Context, domain.FeedFilter) error
}

type SeenRecorder interface {
	MarkEventsSeen(context.Context, []domain.EventID) error
}

type PackageStatusSource interface {
	SetPackageStatus(context.Context, domain.PackageID, domain.PackageStatus) error
}

type FreshnessSource interface {
	LatestFreshness(context.Context) (store.FreshnessStatus, error)
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
	Refresh     tea.Cmd
	Actions     ActionRunner
	Installed   InstalledRefresher
	Send        func(tea.Msg)
	Now         func() time.Time
}

type pane uint8

const (
	feedPane pane = iota
	inspectorPane
)

type Model struct {
	deps                                                              Dependencies
	width, height                                                     int
	groups                                                            []domain.FeedGroup
	selected                                                          int
	showSnoozed                                                       bool
	viewportOffset                                                    int
	focus                                                             pane
	expanded                                                          map[domain.EventID]bool
	filter                                                            domain.FeedFilter
	detailOpen                                                        bool
	loading, stale                                                    bool
	feedRefreshPending                                                bool
	err                                                               error
	notification                                                      string
	light                                                             bool
	feedRequestID, changelogRequestID, detailRequestID, selectionID   uint64
	freshnessRequestID                                                uint64
	notifyRequestID                                                   uint64
	changelog                                                         []store.ChangelogSection
	packageInfo                                                       homebrew.PackageInfo
	packageDescriptions                                               map[domain.PackageID]string
	descriptionRequests                                               map[domain.PackageID]bool
	sessionNew                                                        map[domain.EventID]bool
	seenBoundaryIndex                                                 int
	readme                                                            store.PackageDocument
	repositoryTags                                                    []string
	repositoryTagsExpanded                                            bool
	packageInfoErr, readmeErr                                         error
	repositoryTagsErr                                                 error
	packageInfoLoading, readmeLoading, repositoryTagsLoading          bool
	packageInfoRefreshing, readmeRefreshing, repositoryTagsRefreshing bool
	packageInfoRefreshID, readmeRefreshID, repositoryTagsRefreshID    uint64
	detailSpinnerFrame                                                int
	detailSpinnerRunning                                              bool
	changelogErr                                                      error
	changelogLoading                                                  bool
	changelogArchiveStarted                                           bool
	changelogNextPage                                                 int
	changelogMoreLoading                                              bool
	changelogMoreErr                                                  error
	changelogPageCancel                                               context.CancelFunc
	detailProgress                                                    DetailProgress
	document                                                          store.DocumentKind
	documentExplicit                                                  bool
	feedViewport, inspectorViewport                                   viewport.Model
	refreshAnchors                                                    map[uint64]domain.Anchor
	refreshSelectionIDs                                               map[uint64]uint64
	detailCancel                                                      context.CancelFunc
	initialRefreshRunning                                             bool
	manualRefreshRunning                                              bool
	pendingAction                                                     *homebrew.Action
	actionResult                                                      *homebrew.Action
	actionRunning                                                     bool
	actionOutput                                                      string
	actionAnchor                                                      domain.Anchor
	syncProgress                                                      map[string]SyncProgress
	activeSync                                                        map[string]bool
	searching                                                         bool
	searchQueryCancel                                                 context.CancelFunc
	freshness                                                         store.FreshnessStatus
	freshnessErr                                                      error
}

func New(deps Dependencies) tea.Model { return NewModel(deps) }

func NewModel(deps Dependencies) Model {
	if deps.Context == nil {
		deps.Context = context.Background()
	}
	if deps.Now == nil {
		deps.Now = time.Now
	}
	return Model{deps: deps, expanded: make(map[domain.EventID]bool), loading: true,
		initialRefreshRunning: deps.OnReady != nil,
		feedRequestID:         1, freshnessRequestID: 1, document: store.DocumentChangelog,
		filter: domain.FeedFilter{
			Now: deps.Now(), Kinds: map[domain.EventKind]bool{}, Types: map[domain.PackageType]bool{domain.PackageFormula: true},
			Search: domain.SearchNames,
		},
		feedViewport: viewport.New(), inspectorViewport: viewport.New(), refreshAnchors: make(map[uint64]domain.Anchor), refreshSelectionIDs: make(map[uint64]uint64),
		syncProgress: make(map[string]SyncProgress), activeSync: make(map[string]bool),
		packageDescriptions: make(map[domain.PackageID]string), descriptionRequests: make(map[domain.PackageID]bool),
		sessionNew: make(map[domain.EventID]bool), seenBoundaryIndex: -1}
}

func (m Model) Init() tea.Cmd {
	cmds := []tea.Cmd{m.loadFreshness(m.freshnessRequestID), m.queryFeed(m.feedRequestID)}
	if m.deps.OnReady != nil {
		cmds = append(cmds, m.deps.OnReady)
	}
	if m.deps.Data != nil {
		cmds = append(cmds, m.loadPreferences())
	}
	return tea.Batch(cmds...)
}

func (m Model) Update(msg tea.Msg) (tea.Model, tea.Cmd) {
	switch msg := msg.(type) {
	case detailSpinnerTick:
		if msg.SelectionID != m.selectionID {
			return m, nil
		}
		m.detailSpinnerRunning = false
		if !m.detailFieldsLoading() {
			return m, nil
		}
		m.detailSpinnerFrame = (m.detailSpinnerFrame + 1) % len(detailSpinnerFrames)
		return m, m.startDetailSpinner()
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
		previousEvent := m.selectedEvent()
		anchor, captured := m.refreshAnchors[msg.RequestID]
		selectionAtStart, generationCaptured := m.refreshSelectionIDs[msg.RequestID]
		if !captured {
			anchor = m.anchor()
		}
		// A user interaction after a refresh started must win over the old
		// request anchor. Restoring that anchor unconditionally makes the list
		// jump back to the package that was selected before the interaction.
		if captured && generationCaptured && selectionAtStart != m.selectionID {
			anchor = m.anchor()
		}
		delete(m.refreshAnchors, msg.RequestID)
		delete(m.refreshSelectionIDs, msg.RequestID)
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
			currentEvent := m.selectedEvent()
			if !sameDetailSelection(previousEvent, currentEvent) {
				// The feed can replace the selected event for the same package
				// during a refresh. Treat that as a new detail selection because
				// changelog and README results are event-specific.
				m.selectionID++
				m.resetDetails()
			}
			m.syncViewports()
		}
		cmds := []tea.Cmd{m.markFeedSeen(msg.Groups)}
		if !sameDetailSelection(previousEvent, m.selectedEvent()) {
			cmds = append(cmds, m.detailLoadCommands()...)
		}
		cmds = append(cmds, m.loadVisiblePackageDescriptions()...)
		if m.feedRefreshPending {
			m.feedRefreshPending = false
			next, command := m.refreshDataset()
			m = next
			cmds = append(cmds, command)
		}
		return m, tea.Batch(cmds...)
	case DatasetChanged:
		return m.refreshDataset()
	case InitialRefreshDone:
		m.initialRefreshRunning = false
		return m.refreshDataset()
	case RefreshDone:
		m.manualRefreshRunning = false
		return m.refreshDataset()
	case FreshnessLoaded:
		if msg.RequestID != m.freshnessRequestID {
			return m, nil
		}
		m.freshnessErr = msg.Err
		if msg.Err == nil {
			m.freshness = msg.Status
		}
	case SyncStarted:
		m.activeSync[msg.Source] = true
		m.notification = "Fetching latest " + msg.Source + "…"
	case SyncProgress:
		m.syncProgress[msg.Source] = msg
		m.activeSync[msg.Source] = true
		m.notification = fmt.Sprintf("%s · %d commits scanned · %d updates · %d batches", msg.Source, msg.Commits, msg.Events, msg.Batches)
	case SyncDone:
		delete(m.activeSync, msg.Source)
		m.freshnessRequestID++
		return m, m.loadFreshness(m.freshnessRequestID)
	case PackageStatusSaved:
		if msg.Err != nil {
			m.err = msg.Err
			m.notification = "Error: save package status: " + msg.Err.Error()
			return m, nil
		}
		for groupIndex := range m.groups {
			for eventIndex := range m.groups[groupIndex].Events {
				if m.groups[groupIndex].Events[eventIndex].PackageID == msg.PackageID {
					m.groups[groupIndex].Events[eventIndex].Status = msg.Status
				}
			}
		}
		previousSelection := m.selected
		m.clampSelection()
		m.syncViewports()
		if m.selected != previousSelection {
			return m.selectionChanged()
		}
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
			m.loading = true
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
		if m.hasSelection() {
			m.expanded[m.groups[m.selected].ID] = !m.expanded[m.groups[m.selected].ID]
			m.syncViewports()
		}
	case ChangelogDebounced:
		if msg.SelectionID != m.selectionID || !m.hasSelection() {
			return m, nil
		}
		if m.detailCancel != nil {
			m.detailCancel()
		}
		detailContext, cancel := context.WithCancel(m.deps.Context)
		m.detailCancel = cancel
		m.changelogRequestID++
		m.packageInfoLoading = m.deps.PackageInfo != nil
		m.readmeLoading = m.deps.README != nil
		m.changelogLoading = m.deps.Changelog != nil
		e := m.selectedEvent()
		m.detailRequestID++
		return m, tea.Batch(
			m.loadChangelog(detailContext, m.changelogRequestID, msg.SelectionID, e),
			m.loadPackageInfo(detailContext, m.detailRequestID, msg.SelectionID, e),
			m.loadREADME(detailContext, m.detailRequestID, msg.SelectionID, e),
			m.startDetailSpinner(),
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
		m.packageInfoLoading = false
		m.packageInfoErr = msg.Err
	case READMELoaded:
		e := m.selectedEvent()
		if e.PackageID != msg.PackageID || (msg.EventID != "" && e.ID != msg.EventID) ||
			(msg.RequestID != 0 && (msg.RequestID != m.detailRequestID || msg.SelectionID != m.selectionID)) {
			return m, nil
		}
		if msg.Err == nil {
			m.readme = msg.Document
			if len(m.changelog) == 0 && msg.Document.ID != "" && !m.documentExplicit {
				m.document = store.DocumentREADME
			}
		}
		m.readmeLoading = false
		m.readmeErr = msg.Err
	case RepositoryTagsLoaded:
		if m.selectedEvent().PackageID != msg.PackageID ||
			(msg.SelectionID != 0 && msg.SelectionID != m.selectionID) {
			return m, nil
		}
		if msg.Err == nil {
			m.repositoryTags = append([]string(nil), msg.Record.Tags...)
		}
		m.repositoryTagsLoading = false
		m.repositoryTagsErr = msg.Err
	case DetailFieldLoading:
		if m.selectedEvent().PackageID != msg.PackageID {
			return m, nil
		}
		switch msg.Field {
		case DetailPackageInfo:
			if msg.Loading && msg.RequestID != 0 && msg.RequestID < m.packageInfoRefreshID {
				return m, nil
			}
			if !msg.Loading && msg.RequestID != 0 && msg.RequestID != m.packageInfoRefreshID {
				return m, nil
			}
			if msg.RequestID != 0 {
				m.packageInfoRefreshID = msg.RequestID
			}
			m.packageInfoRefreshing = msg.Loading
		case DetailREADME:
			if msg.Loading && msg.RequestID != 0 && msg.RequestID < m.readmeRefreshID {
				return m, nil
			}
			if !msg.Loading && msg.RequestID != 0 && msg.RequestID != m.readmeRefreshID {
				return m, nil
			}
			if msg.RequestID != 0 {
				m.readmeRefreshID = msg.RequestID
			}
			m.readmeRefreshing = msg.Loading
		case DetailRepositoryTags:
			if msg.Loading && msg.RequestID != 0 && msg.RequestID < m.repositoryTagsRefreshID {
				return m, nil
			}
			if !msg.Loading && msg.RequestID != 0 && msg.RequestID != m.repositoryTagsRefreshID {
				return m, nil
			}
			if msg.RequestID != 0 {
				m.repositoryTagsRefreshID = msg.RequestID
			}
			m.repositoryTagsRefreshing = msg.Loading
		}
		if msg.Loading {
			return m, m.startDetailSpinner()
		}
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
		m.captureRefreshAnchor(m.feedRequestID, m.actionAnchor)
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
	if key.Key().Code == 's' && key.Key().Mod == tea.ModAlt|tea.ModShift {
		return m.toggleSnoozedVisibility()
	}
	if key.Key().Code == 's' && key.Key().Mod == tea.ModAlt {
		return m.cycleSelectedPackageStatus()
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
	case "r":
		if m.deps.Refresh == nil || m.manualRefreshRunning {
			return m, nil
		}
		m.manualRefreshRunning = true
		m.notification = "Refreshing latest updates…"
		return m, m.deps.Refresh
	case "t":
		m.repositoryTagsExpanded = !m.repositoryTagsExpanded
		m.syncViewports()
	case "/":
		m.searching = true
		m.focus = feedPane
		m.detailOpen = false
		rollUpChanged := !m.filter.RollUp
		m.filter.RollUp = true
		if !validSearchScope(m.filter.Search) {
			m.filter.Search = domain.SearchNames
		}
		m.syncViewports()
		if rollUpChanged {
			return m.searchChanged()
		}
		return m, nil
	case "j", "down":
		return m.moveSelection(1)
	case "k", "up":
		return m.moveSelection(-1)
	case "h", "left":
		if m.width >= 100 {
			m.focus = feedPane
		} else {
			m.detailOpen = false
		}
	case "l", "right":
		if m.width >= 100 {
			m.focus = inspectorPane
		} else if m.hasSelection() {
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
		if m.width < 100 && m.hasSelection() {
			m.detailOpen = true
		}
		if m.width >= 100 {
			m.focus = inspectorPane
			m.inspectorViewport.SetYOffset(0)
			m.syncViewports()
		}
	case " ":
		if m.hasSelection() {
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
	m.loading = true
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
	if !m.hasSelection() {
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
	if !m.selectableFeedIndex(index) || index == m.selected {
		return m, nil
	}
	m.selected = index
	return m.selectionChanged()
}

func (m Model) selectionChanged() (tea.Model, tea.Cmd) {
	m.selectionID++
	m.resetDetails()
	if !m.hasSelection() {
		m.viewportOffset = 0
		m.feedViewport.SetYOffset(0)
		m.syncViewports()
		return m, tea.Batch(m.loadVisiblePackageDescriptions()...)
	}
	m.keepSelectionVisible()
	commands := m.detailLoadCommands()
	commands = append(commands, m.loadVisiblePackageDescriptions()...)
	return m, tea.Batch(commands...)
}

func (m Model) moveSelection(direction int) (tea.Model, tea.Cmd) {
	start := m.selected + direction
	if m.selected < 0 && direction < 0 {
		start = len(m.groups) - 1
	}
	next := m.nextSelectableFeedIndex(start, direction)
	if next < 0 {
		return m, nil
	}
	m.selected = next
	m.selectionID++
	m.resetDetails()
	m.keepSelectionVisible()
	// Keyboard navigation can move through many rows in one burst. Keep the
	// detail pane in a local loading state, but defer cache reads and remote
	// admission until the selection debounce settles.
	m.startCachedDetailLoads()
	commands := []tea.Cmd{m.debounceChangelog()}
	commands = append(commands, m.loadVisiblePackageDescriptions()...)
	return m, tea.Batch(commands...)
}

func (m Model) toggleSnoozedVisibility() (tea.Model, tea.Cmd) {
	m.showSnoozed = !m.showSnoozed
	previousSelection := m.selected
	m.clampSelection()
	m.syncViewports()
	if m.selected != previousSelection {
		return m.selectionChanged()
	}
	return m, tea.Batch(m.loadVisiblePackageDescriptions()...)
}

func (m *Model) setPackageScope(formulae, casks bool) {
	m.filter.Types = map[domain.PackageType]bool{
		domain.PackageFormula: formulae,
		domain.PackageCask:    casks,
	}
}

func (m Model) filterChanged() (tea.Model, tea.Cmd) {
	m.cancelSearchQuery()
	m.loading = true
	m.feedRequestID++
	return m, tea.Batch(m.queryFeed(m.feedRequestID), m.savePreferences())
}

func (m Model) refreshDataset() (Model, tea.Cmd) {
	m.stale = true
	if m.loading {
		// A newer commit does not make an in-flight query useless. Display its
		// result, then query again once for all commits that arrived meanwhile.
		// Invalidating every response can starve the view during steady indexing.
		m.feedRefreshPending = true
		return m, nil
	}
	m.loading = true
	m.feedRequestID++
	m.freshnessRequestID++
	m.captureRefreshAnchor(m.feedRequestID, m.anchor())
	return m, tea.Batch(m.queryFeed(m.feedRequestID), m.loadFreshness(m.freshnessRequestID))
}

func (m Model) cycleSelectedPackageStatus() (tea.Model, tea.Cmd) {
	if !m.hasSelection() {
		return m, nil
	}
	source, ok := m.deps.Data.(PackageStatusSource)
	if !ok {
		return m, nil
	}
	event := m.selectedEvent()
	status := domain.NextPackageStatus(event.Status)
	return m, func() tea.Msg {
		return PackageStatusSaved{PackageID: event.PackageID, Status: status, Err: source.SetPackageStatus(m.deps.Context, event.PackageID, status)}
	}
}

func (m Model) anchor() domain.Anchor {
	if !m.hasSelection() {
		return domain.Anchor{FallbackIndex: m.selected, ViewportOffset: m.viewportOffset}
	}
	e := m.selectedEvent()
	return domain.Anchor{GroupID: m.groups[m.selected].ID, ChildEventID: e.ID, FallbackIndex: m.selected, ViewportOffset: m.viewportOffset}
}

func (m *Model) captureRefreshAnchor(requestID uint64, anchor domain.Anchor) {
	if m.refreshAnchors == nil {
		m.refreshAnchors = make(map[uint64]domain.Anchor)
	}
	if m.refreshSelectionIDs == nil {
		m.refreshSelectionIDs = make(map[uint64]uint64)
	}
	m.refreshAnchors[requestID] = anchor
	m.refreshSelectionIDs[requestID] = m.selectionID
}

func (m *Model) clampSelection() {
	if len(m.groups) == 0 {
		m.selected = -1
		return
	}
	if m.selected < 0 {
		m.selected = 0
	}
	if m.selected >= len(m.groups) {
		m.selected = len(m.groups) - 1
	}
	if m.selectableFeedIndex(m.selected) {
		return
	}
	if next := m.nextSelectableFeedIndex(m.selected+1, 1); next >= 0 {
		m.selected = next
		return
	}
	if next := m.nextSelectableFeedIndex(m.selected-1, -1); next >= 0 {
		m.selected = next
		return
	}
	m.selected = -1
}

func (m Model) hasSelection() bool { return m.selectableFeedIndex(m.selected) }

func (m Model) selectableFeedIndex(index int) bool {
	return index >= 0 && index < len(m.groups) && !m.snoozedGroupCollapsed(m.groups[index])
}

func (m Model) nextSelectableFeedIndex(start, direction int) int {
	for index := start; index >= 0 && index < len(m.groups); index += direction {
		if m.selectableFeedIndex(index) {
			return index
		}
	}
	return -1
}

func (m Model) selectedEvent() domain.UpdateEvent {
	if !m.hasSelection() {
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
	m.packageInfoLoading = false
	m.readmeLoading = false
	m.repositoryTagsLoading = false
	m.changelogLoading = false
	m.packageInfoRefreshing = false
	m.readmeRefreshing = false
	m.repositoryTagsRefreshing = false
	m.detailSpinnerFrame = 0
	m.detailSpinnerRunning = false
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

func sameDetailSelection(a, b domain.UpdateEvent) bool {
	return a.PackageID != "" && a.PackageID == b.PackageID && a.ID == b.ID
}

// detailLoadCommands starts the cached-first pipeline for the current
// selection exactly once per selection change. Feed refreshes that leave the
// selected package/event intact deliberately do not call this method.
func (m *Model) detailLoadCommands() []tea.Cmd {
	if !m.hasSelection() {
		return nil
	}
	m.startCachedDetailLoads()
	e := m.selectedEvent()
	commands := []tea.Cmd{
		m.loadCachedPackageInfo(m.selectionID, e),
		m.loadCachedREADME(m.selectionID, e),
		m.loadCachedChangelog(m.selectionID, e),
		m.loadCachedRepositoryTags(m.selectionID, e),
		m.debounceChangelog(),
		m.startDetailSpinner(),
	}
	return commands
}

func (m *Model) startCachedDetailLoads() {
	if m.selectedEvent().PackageID == "" {
		return
	}
	_, m.packageInfoLoading = m.deps.PackageInfo.(CachedPackageInfoSource)
	_, m.readmeLoading = m.deps.README.(CachedREADMESource)
	_, m.changelogLoading = m.deps.Changelog.(CachedChangelogSource)
	m.repositoryTagsLoading = m.deps.Tags != nil
}

func (m Model) queryFeed(id uint64) tea.Cmd {
	return m.queryFeedContext(m.deps.Context, id)
}

func (m Model) loadFreshness(id uint64) tea.Cmd {
	return func() tea.Msg {
		source, ok := m.deps.Data.(FreshnessSource)
		if !ok {
			return FreshnessLoaded{RequestID: id}
		}
		status, err := source.LatestFreshness(m.deps.Context)
		return FreshnessLoaded{RequestID: id, Status: status, Err: err}
	}
}

func (m Model) queryFeedContext(ctx context.Context, id uint64) tea.Cmd {
	return func() tea.Msg {
		if m.deps.Data == nil {
			return FeedLoaded{RequestID: id}
		}
		filter := m.filter
		filter.Now = m.deps.Now()
		g, e := m.deps.Data.QueryFeed(ctx, filter)
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
	if m.initialRefreshRunning {
		return boundary >= 0 && boundary < len(m.groups)
	}
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

func (m Model) detailFieldsLoading() bool {
	return m.packageInfoLoading || m.packageInfoRefreshing ||
		m.readmeLoading || m.readmeRefreshing ||
		m.repositoryTagsLoading || m.repositoryTagsRefreshing ||
		m.changelogLoading || m.changelogMoreLoading
}

func (m *Model) startDetailSpinner() tea.Cmd {
	if m.detailSpinnerRunning || !m.detailFieldsLoading() {
		return nil
	}
	m.detailSpinnerRunning = true
	selection := m.selectionID
	return tea.Tick(detailSpinInterval, func(time.Time) tea.Msg {
		return detailSpinnerTick{SelectionID: selection}
	})
}

func (m Model) detailSpinner(loading bool) string {
	if !loading {
		return ""
	}
	return " " + detailSpinnerFrames[m.detailSpinnerFrame%len(detailSpinnerFrames)]
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
		info, _, err := source.LoadCachedPackageInfo(m.deps.Context, e.PackageID)
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
		height := m.feedGroupHeight(group, m.feedViewport.Width())
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
		document, _, err := source.LoadCachedREADME(m.deps.Context, e.PackageID)
		return READMELoaded{SelectionID: selection, PackageID: e.PackageID, EventID: e.ID, Document: document, Err: err}
	}
}

func (m Model) loadCachedRepositoryTags(selection uint64, e domain.UpdateEvent) tea.Cmd {
	return func() tea.Msg {
		if e.PackageID == "" || m.deps.Tags == nil {
			return nil
		}
		record, _, err := m.deps.Tags.LoadCachedRepositoryTags(m.deps.Context, e.PackageID)
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
		return READMELoaded{RequestID: request, SelectionID: selection, PackageID: e.PackageID, EventID: e.ID, Document: document, Err: err}
	}
}
