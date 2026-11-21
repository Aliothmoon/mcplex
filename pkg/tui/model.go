package tui

import (
	"fmt"
	"regexp"
	"strings"
	"time"

	tea "github.com/charmbracelet/bubbletea"
	"github.com/charmbracelet/x/ansi"

	"github.com/aliothmoon/mcplex/pkg/diff"
	"github.com/aliothmoon/mcplex/pkg/events"
)

// Config configures the interactive MCPlex TUI header and connection info.
type Config struct {
	Endpoint    string
	Version     string
	ModeLabel   string // "local" | "remote"
	AuthEnabled bool
	StrictRoot  bool
}

type focusArea int

const (
	focusToolbar focusArea = iota
	focusFeed
)

type filterTab int

const (
	tabAll filterTab = iota
	tabCRUD
	tabReadSearch
	tabShellTask
)

type toolbarControl struct {
	id    string
	label string
	isTab bool
	tab   filterTab
}

type hitZone struct {
	x1, x2 int
	action string
	idx    int
}

type rowTarget struct {
	nodeID string
	action string // "toggle" | "inspect" | "select"
}

type childItem struct {
	id       string
	ev       events.Event
	expanded bool
}

type runGroup struct {
	id         string
	runID      string
	isExec     bool
	startEv    events.Event
	endEv      *events.Event
	children   []*childItem
	expanded   bool
	addedSum   int
	removedSum int
}

type streamRef struct {
	nodeID        string
	group         *runGroup
	child         *childItem
	countedActive bool
}

func (sr *streamRef) eventPtr() *events.Event {
	if sr.child != nil {
		return &sr.child.ev
	}
	return &sr.group.startEv
}

type detailLine struct {
	gutter string
	plain  string
	styled string
	bgHex  string
}

type modalRect struct {
	x, y, w, h int
}

type detailState struct {
	open        bool
	streamID    string
	streaming   bool
	followTail  bool
	title       string
	badgeText   string
	absPath     string
	oldAbsPath  string
	cwd         string
	metaLine    string
	lines       []detailLine
	scroll      int
	searchMode  bool
	searchInput string
	query       string
	matches     []int
	matchCursor int
	btnIdx      int // focused button index in popup bottom action bar (0..4)
	hoverBtn    int // hovered button index (-1=none, -2=close chip)
	rect        modalRect
	closeZone   hitZone   // clickable [ × Esc ] on modal top border row
	actionZones []hitZone // clickable buttons on modal bottom action row
}

// Model is the Bubble Tea state machine for the Grok-Build-CLI dark interactive MCPlex console.
type Model struct {
	cfg       Config
	source    EventSource
	connected bool
	connNote  string

	width  int
	height int

	focus      focusArea
	toolbarIdx int

	groups      []*runGroup
	groupByRun  map[string]*runGroup
	streamByID  map[string]*streamRef
	nodeOrder   []string
	selectedID  string
	scroll      int
	follow      bool
	activeTab   filterTab
	sessionList []string
	sessionIdx  int

	// Feed search state (/ + n / N)
	searchMode  bool
	searchInput string
	feedQuery   string
	feedMatches []string
	feedMatchAt int

	// Cached layout & mouse hit-testing / hover state
	tabZones     []hitZone
	rowTargets   []rowTarget
	totalVisRows int
	bodyTopY     int
	bodyHeight   int
	hoverToolbar int
	hoverNodeID  string

	// Double-click detection
	lastClickID   string
	lastClickTime time.Time

	// Aggregate counters
	totalRuns  int
	totalCRUD  int
	totalTasks int
	activeRuns int
	totalAdd   int
	totalDel   int

	// Floating overlay detail popup (less-style scroll + search + full absolute path)
	detail detailState

	// On-demand braille spinner (only ticks while exec/shell/task is actively running)
	spinnerFrame int
	tickActive   bool
}

type spinnerTickMsg struct{}

var spinnerFrames = []string{"⠋", "⠙", "⠹", "⠸", "⠼", "⠴", "⠦", "⠧", "⠇", "⠏"}

func spinnerTickCmd() tea.Cmd {
	return tea.Tick(100*time.Millisecond, func(time.Time) tea.Msg {
		return spinnerTickMsg{}
	})
}

func (m *Model) hasActiveWork() bool {
	if m.activeRuns > 0 {
		return true
	}
	for _, sr := range m.streamByID {
		if sr != nil && sr.eventPtr().Status == "running" {
			return true
		}
	}
	return false
}

func (m *Model) currentSpinner() string {
	return spinnerFrames[m.spinnerFrame%len(spinnerFrames)]
}

// NewModel creates a new TUI model backed by source (local EventHub or remote SSE).
func NewModel(cfg Config, src EventSource) *Model {
	return &Model{
		cfg:          cfg,
		source:       src,
		width:        120,
		height:       36,
		focus:        focusToolbar,
		toolbarIdx:   0,
		groupByRun:   map[string]*runGroup{},
		streamByID:   map[string]*streamRef{},
		follow:       true,
		connected:    true,
		bodyTopY:     3,
		bodyHeight:   30,
		hoverToolbar: -1,
		detail:       detailState{hoverBtn: -1},
	}
}

func (m *Model) toolbarControls() []toolbarControl {
	ctrls := []toolbarControl{
		{id: "tab_all", label: "All", isTab: true, tab: tabAll},
		{id: "tab_crud", label: "CRUD", isTab: true, tab: tabCRUD},
		{id: "tab_read", label: "Read", isTab: true, tab: tabReadSearch},
		{id: "tab_shell", label: "Shell/Task", isTab: true, tab: tabShellTask},
	}
	if len(m.sessionList) > 0 {
		label := "Sess:all"
		if sf := m.activeSessionFilter(); sf != "" {
			short := sf
			if len(short) > 10 {
				short = short[:10] + "…"
			}
			label = "Sess:" + short
		}
		ctrls = append(ctrls, toolbarControl{id: "session", label: label})
	}
	followLbl := "Follow:OFF"
	if m.follow {
		followLbl = "Follow:ON"
	}
	ctrls = append(ctrls,
		toolbarControl{id: "expand_all", label: "Expand"},
		toolbarControl{id: "collapse_all", label: "Collapse"},
		toolbarControl{id: "follow", label: followLbl},
		toolbarControl{id: "search", label: "Find"},
		toolbarControl{id: "clear", label: "Clear"},
	)
	return ctrls
}

func (m *Model) Init() tea.Cmd {
	if m.source == nil {
		return nil
	}
	return m.waitNextEvent()
}

func (m *Model) waitNextEvent() tea.Cmd {
	if m.source == nil {
		return nil
	}
	ch := m.source.Messages()
	return func() tea.Msg {
		msg, ok := <-ch
		if !ok {
			return sourceClosedMsg{}
		}
		return msg
	}
}

func (m *Model) Update(msg tea.Msg) (tea.Model, tea.Cmd) {
	switch msg := msg.(type) {
	case tea.WindowSizeMsg:
		m.width = max(44, msg.Width)
		m.height = max(14, msg.Height)
		m.recomputeLayoutMetrics()
		m.syncFeedLayout()
		return m, nil

	case eventMsg:
		m.ingestEvent(msg.Event)
		if m.hasActiveWork() && !m.tickActive {
			m.tickActive = true
			return m, tea.Batch(m.waitNextEvent(), spinnerTickCmd())
		}
		return m, m.waitNextEvent()

	case spinnerTickMsg:
		if m.hasActiveWork() {
			m.spinnerFrame = (m.spinnerFrame + 1) % len(spinnerFrames)
			return m, spinnerTickCmd()
		}
		m.tickActive = false
		return m, nil

	case connStatusMsg:
		m.connected = msg.Connected
		m.connNote = msg.Note
		return m, m.waitNextEvent()

	case sourceClosedMsg:
		m.connected = false
		m.connNote = "disconnected"
		return m, nil

	case tea.MouseMsg:
		return m.handleMouse(msg), nil

	case tea.KeyMsg:
		return m.handleKey(msg)
	}
	return m, nil
}

func (m *Model) recomputeLayoutMetrics() {
	m.bodyTopY = 3
	m.bodyHeight = max(4, m.height-4)
}

func (m *Model) modalPageHeight() int {
	_, _, _, mh := m.computeModalBounds()
	// Overhead inside modal: top border (1) + meta rows (2) + divider (1) + divider (1) + action bar (1) + bottom border (1) = 7
	return max(3, mh-7)
}

func (m *Model) computeModalBounds() (x, y, w, h int) {
	w = min(m.width-4, max(66, (m.width*86)/100))
	if w > m.width-2 {
		w = max(36, m.width-2)
	}
	h = min(m.height-4, max(14, (m.height*82)/100))
	if h > m.height-2 {
		h = max(10, m.height-2)
	}
	x = max(1, (m.width-w)/2)
	y = max(1, (m.height-h)/2)
	return x, y, w, h
}

func (m *Model) handleKey(msg tea.KeyMsg) (tea.Model, tea.Cmd) {
	if msg.Type == tea.KeyCtrlC || msg.String() == "ctrl+c" {
		return m, tea.Quit
	}
	if m.detail.open {
		return m.handleDetailKey(msg)
	}
	if m.searchMode {
		return m.handleFeedSearchKey(msg)
	}

	switch msg.String() {
	case "q":
		return m, tea.Quit
	case "1":
		m.selectFilterTab(tabAll)
		return m, nil
	case "2":
		m.selectFilterTab(tabCRUD)
		return m, nil
	case "3":
		m.selectFilterTab(tabReadSearch)
		return m, nil
	case "4":
		m.selectFilterTab(tabShellTask)
		return m, nil
	case "/":
		m.searchMode = true
		m.searchInput = m.feedQuery
		return m, nil
	case "f":
		m.toggleFollow()
		return m, nil
	case "e":
		m.setAllExpanded(true)
		m.syncFeedLayout()
		m.ensureSelectedVisible()
		return m, nil
	case "c":
		m.setAllExpanded(false)
		m.syncFeedLayout()
		m.snapSelectionToVisible()
		return m, nil
	case "x":
		m.clearFeed()
		return m, nil
	case "n":
		m.jumpFeedMatch(1)
		return m, nil
	case "N":
		m.jumpFeedMatch(-1)
		return m, nil
	}

	if msg.Type == tea.KeyTab {
		m.syncFeedLayout()
		if m.focus == focusToolbar && len(m.nodeOrder) > 0 {
			m.focus = focusFeed
			m.snapSelectionToVisible()
		} else if m.focus == focusFeed {
			m.focus = focusToolbar
		} else {
			m.moveToolbarCursor(1)
		}
		return m, nil
	}
	if msg.Type == tea.KeyShiftTab {
		m.syncFeedLayout()
		if m.focus == focusFeed {
			m.focus = focusToolbar
		} else if len(m.nodeOrder) > 0 {
			m.focus = focusFeed
			m.snapSelectionToVisible()
		} else {
			m.moveToolbarCursor(-1)
		}
		return m, nil
	}

	if msg.Type == tea.KeyEsc {
		if m.feedQuery != "" {
			m.feedQuery = ""
			m.feedMatches = nil
		}
		return m, nil
	}

	m.syncFeedLayout()
	if len(m.nodeOrder) == 0 {
		m.focus = focusToolbar
	}

	if m.focus == focusToolbar {
		return m.handleToolbarKey(msg)
	}
	return m.handleFeedKey(msg)
}

func (m *Model) handleToolbarKey(msg tea.KeyMsg) (tea.Model, tea.Cmd) {
	switch msg.Type {
	case tea.KeyLeft:
		m.moveToolbarCursor(-1)
		return m, nil
	case tea.KeyRight:
		m.moveToolbarCursor(1)
		return m, nil
	case tea.KeyDown:
		if len(m.nodeOrder) > 0 {
			m.focus = focusFeed
			m.follow = false
			if m.selectedID == "" {
				m.selectedID = m.nodeOrder[0]
			}
			m.snapSelectionToVisible()
		}
		return m, nil
	case tea.KeyEnter:
		m.activateToolbarControl(m.toolbarIdx)
		return m, nil
	}

	switch msg.String() {
	case "h":
		m.moveToolbarCursor(-1)
	case "l":
		m.moveToolbarCursor(1)
	case "j":
		if len(m.nodeOrder) > 0 {
			m.focus = focusFeed
			m.follow = false
			m.snapSelectionToVisible()
		}
	case " ":
		m.activateToolbarControl(m.toolbarIdx)
	}
	return m, nil
}

func (m *Model) handleFeedKey(msg tea.KeyMsg) (tea.Model, tea.Cmd) {
	switch msg.Type {
	case tea.KeyUp:
		m.follow = false
		m.moveSelection(-1)
		return m, nil
	case tea.KeyDown:
		m.follow = false
		m.moveSelection(1)
		return m, nil
	case tea.KeyLeft:
		m.follow = false
		m.collapseOrParent()
		return m, nil
	case tea.KeyRight:
		m.follow = false
		m.expandOrInspect()
		return m, nil
	case tea.KeyPgUp, tea.KeyCtrlU:
		m.follow = false
		m.scrollFeed(-max(3, m.bodyHeight/2))
		return m, nil
	case tea.KeyPgDown, tea.KeyCtrlD:
		m.follow = false
		m.scrollFeed(max(3, m.bodyHeight/2))
		return m, nil
	case tea.KeyHome:
		m.follow = false
		m.scroll = 0
		if len(m.nodeOrder) > 0 {
			m.selectedID = m.nodeOrder[0]
		}
		return m, nil
	case tea.KeyEnd:
		m.follow = true
		if len(m.nodeOrder) > 0 {
			m.selectedID = m.nodeOrder[len(m.nodeOrder)-1]
		}
		m.scrollToBottom()
		return m, nil
	case tea.KeyEnter:
		if m.selectedID != "" {
			m.follow = false
			m.toggleNode(m.selectedID)
			m.syncFeedLayout()
			m.ensureSelectedVisible()
		}
		return m, nil
	}

	switch msg.String() {
	case "k":
		m.follow = false
		m.moveSelection(-1)
	case "j":
		m.follow = false
		m.moveSelection(1)
	case "h":
		m.follow = false
		m.collapseOrParent()
	case "l":
		m.follow = false
		m.expandOrInspect()
	case " ":
		if m.selectedID != "" {
			m.follow = false
			m.toggleNode(m.selectedID)
			m.syncFeedLayout()
			m.ensureSelectedVisible()
		}
	case "o":
		if m.selectedID != "" {
			m.openDetailForNode(m.selectedID)
		}
	case "b":
		m.follow = false
		m.scrollFeed(-max(3, m.bodyHeight/2))
	case "g":
		m.follow = false
		m.scroll = 0
		if len(m.nodeOrder) > 0 {
			m.selectedID = m.nodeOrder[0]
		}
	case "G":
		m.follow = true
		if len(m.nodeOrder) > 0 {
			m.selectedID = m.nodeOrder[len(m.nodeOrder)-1]
		}
		m.scrollToBottom()
	case "d":
		m.selectFilterTab(tabCRUD)
	case "s":
		m.cycleSession()
	}
	return m, nil
}

func (m *Model) moveToolbarCursor(delta int) {
	ctrls := m.toolbarControls()
	if len(ctrls) == 0 {
		return
	}
	m.toolbarIdx = (m.toolbarIdx + delta + len(ctrls)) % len(ctrls)
	if ctrls[m.toolbarIdx].isTab {
		m.activeTab = ctrls[m.toolbarIdx].tab
		m.syncFeedLayout()
		m.snapSelectionToVisible()
		m.rebuildFeedMatches()
	}
}

func (m *Model) selectFilterTab(t filterTab) {
	m.activeTab = t
	ctrls := m.toolbarControls()
	for i, c := range ctrls {
		if c.isTab && c.tab == t {
			m.toolbarIdx = i
			break
		}
	}
	m.syncFeedLayout()
	m.snapSelectionToVisible()
	m.rebuildFeedMatches()
}

func (m *Model) activateToolbarControl(idx int) {
	ctrls := m.toolbarControls()
	if idx < 0 || idx >= len(ctrls) {
		return
	}
	c := ctrls[idx]
	m.toolbarIdx = idx
	switch c.id {
	case "tab_all", "tab_crud", "tab_read", "tab_shell":
		m.activeTab = c.tab
		m.syncFeedLayout()
		m.snapSelectionToVisible()
		m.rebuildFeedMatches()
		if len(m.nodeOrder) > 0 {
			m.focus = focusFeed
		}
	case "session":
		m.cycleSession()
	case "expand_all":
		m.setAllExpanded(true)
		m.syncFeedLayout()
		m.ensureSelectedVisible()
	case "collapse_all":
		m.setAllExpanded(false)
		m.syncFeedLayout()
		m.snapSelectionToVisible()
	case "follow":
		m.toggleFollow()
	case "search":
		m.searchMode = true
		m.searchInput = m.feedQuery
	case "clear":
		m.clearFeed()
	}
}

func (m *Model) toggleFollow() {
	m.follow = !m.follow
	if m.follow {
		m.syncFeedLayout()
		if len(m.nodeOrder) > 0 {
			m.selectedID = m.nodeOrder[len(m.nodeOrder)-1]
		}
		m.scrollToBottom()
	}
}

func (m *Model) cycleSession() {
	if len(m.sessionList) == 0 {
		return
	}
	m.sessionIdx = (m.sessionIdx + 1) % (len(m.sessionList) + 1)
	m.syncFeedLayout()
	m.snapSelectionToVisible()
	m.rebuildFeedMatches()
}

func (m *Model) handleFeedSearchKey(msg tea.KeyMsg) (tea.Model, tea.Cmd) {
	switch msg.Type {
	case tea.KeyEsc:
		m.searchMode = false
		return m, nil
	case tea.KeyEnter:
		m.searchMode = false
		m.feedQuery = strings.TrimSpace(m.searchInput)
		m.rebuildFeedMatches()
		if len(m.feedMatches) > 0 {
			m.feedMatchAt = 0
			m.selectedID = m.feedMatches[0]
			m.focus = focusFeed
			m.follow = false
			m.ensureSelectedVisible()
		}
		return m, nil
	case tea.KeyBackspace:
		r := []rune(m.searchInput)
		if len(r) > 0 {
			m.searchInput = string(r[:len(r)-1])
		}
		return m, nil
	case tea.KeyRunes:
		m.searchInput += msg.String()
		return m, nil
	}
	if msg.String() == " " {
		m.searchInput += " "
	}
	return m, nil
}

const modalActionCount = 5 // Search, Prev, Next, Top, End

func (m *Model) triggerDetailAction(idx int) {
	page := m.modalPageHeight()
	switch idx {
	case 0: // / Search
		m.detail.searchMode = true
		m.detail.searchInput = m.detail.query
	case 1: // Prev match
		m.stepDetailMatch(-1)
	case 2: // Next match
		m.stepDetailMatch(1)
	case 3: // Top
		m.detail.scroll = 0
		m.detail.followTail = false
	case 4: // End
		m.detail.scroll = max(0, len(m.detail.lines)-page)
		m.detail.followTail = true
	}
}

func (m *Model) handleDetailKey(msg tea.KeyMsg) (tea.Model, tea.Cmd) {
	if m.detail.searchMode {
		switch msg.Type {
		case tea.KeyEsc:
			m.detail.searchMode = false
			return m, nil
		case tea.KeyEnter:
			m.detail.searchMode = false
			m.detail.query = strings.TrimSpace(m.detail.searchInput)
			m.recomputeDetailMatches()
			if len(m.detail.matches) > 0 {
				m.detail.matchCursor = 0
				m.jumpDetailToMatch(0)
			}
			return m, nil
		case tea.KeyBackspace:
			r := []rune(m.detail.searchInput)
			if len(r) > 0 {
				m.detail.searchInput = string(r[:len(r)-1])
			}
			m.detail.query = strings.TrimSpace(m.detail.searchInput)
			m.recomputeDetailMatches()
			return m, nil
		case tea.KeyRunes:
			m.detail.searchInput += msg.String()
			m.detail.query = strings.TrimSpace(m.detail.searchInput)
			m.recomputeDetailMatches()
			return m, nil
		}
		return m, nil
	}

	page := m.modalPageHeight()
	switch msg.Type {
	case tea.KeyEsc:
		if m.detail.query != "" {
			m.detail.query = ""
			m.detail.searchInput = ""
			m.detail.matches = nil
		} else {
			m.detail.open = false
		}
		return m, nil
	case tea.KeyDown:
		m.scrollDetail(1)
		return m, nil
	case tea.KeyUp:
		m.scrollDetail(-1)
		return m, nil
	case tea.KeyLeft, tea.KeyShiftTab:
		m.detail.btnIdx = (m.detail.btnIdx - 1 + modalActionCount) % modalActionCount
		return m, nil
	case tea.KeyRight, tea.KeyTab:
		m.detail.btnIdx = (m.detail.btnIdx + 1) % modalActionCount
		return m, nil
	case tea.KeyEnter:
		m.triggerDetailAction(m.detail.btnIdx)
		return m, nil
	case tea.KeyPgDown:
		m.scrollDetail(page)
		return m, nil
	case tea.KeyPgUp:
		m.scrollDetail(-page)
		return m, nil
	case tea.KeyCtrlD:
		m.scrollDetail(max(2, page/2))
		return m, nil
	case tea.KeyCtrlU:
		m.scrollDetail(-max(2, page/2))
		return m, nil
	case tea.KeyHome:
		m.detail.scroll = 0
		m.detail.followTail = false
		return m, nil
	case tea.KeyEnd:
		m.detail.scroll = max(0, len(m.detail.lines)-page)
		m.detail.followTail = true
		return m, nil
	}

	switch msg.String() {
	case "q", "o":
		m.detail.open = false
	case "j":
		m.scrollDetail(1)
	case "k":
		m.scrollDetail(-1)
	case "h":
		m.detail.btnIdx = (m.detail.btnIdx - 1 + modalActionCount) % modalActionCount
	case "l":
		m.detail.btnIdx = (m.detail.btnIdx + 1) % modalActionCount
	case " ", "f":
		m.scrollDetail(page)
	case "b":
		m.scrollDetail(-page)
	case "d":
		m.scrollDetail(max(2, page/2))
	case "u":
		m.scrollDetail(-max(2, page/2))
	case "g":
		m.detail.scroll = 0
		m.detail.followTail = false
	case "G":
		m.detail.scroll = max(0, len(m.detail.lines)-page)
		m.detail.followTail = true
	case "/":
		m.detail.searchMode = true
		m.detail.searchInput = m.detail.query
	case "n":
		m.stepDetailMatch(1)
	case "N":
		m.stepDetailMatch(-1)
	}
	return m, nil
}

func (m *Model) handleMouse(msg tea.MouseMsg) tea.Model {
	if msg.Button == tea.MouseButtonWheelUp {
		if m.detail.open {
			m.scrollDetail(-3)
		} else {
			m.follow = false
			m.scrollFeed(-3)
		}
		return m
	}
	if msg.Button == tea.MouseButtonWheelDown {
		if m.detail.open {
			m.scrollDetail(3)
		} else {
			m.scrollFeed(3)
		}
		return m
	}

	if msg.Action == tea.MouseActionMotion {
		if m.detail.open {
			m.detail.hoverBtn = -1
			r := m.detail.rect
			if msg.Y == r.y && msg.X >= m.detail.closeZone.x1 && msg.X < m.detail.closeZone.x2 {
				m.detail.hoverBtn = -2
				return m
			}
			if msg.Y == r.y+r.h-2 {
				for _, z := range m.detail.actionZones {
					if msg.X >= z.x1 && msg.X < z.x2 {
						m.detail.hoverBtn = z.idx
						break
					}
				}
			}
			return m
		}
		m.hoverToolbar = -1
		m.hoverNodeID = ""
		if msg.Y == 1 {
			for _, z := range m.tabZones {
				if msg.X >= z.x1 && msg.X < z.x2 {
					m.hoverToolbar = z.idx
					break
				}
			}
		} else if msg.Y >= m.bodyTopY && msg.Y < m.bodyTopY+m.bodyHeight {
			visIdx := m.scroll + (msg.Y - m.bodyTopY)
			if visIdx >= 0 && visIdx < len(m.rowTargets) {
				m.hoverNodeID = m.rowTargets[visIdx].nodeID
			}
		}
		return m
	}

	if msg.Action != tea.MouseActionPress || msg.Button != tea.MouseButtonLeft {
		return m
	}

	if m.detail.open {
		r := m.detail.rect
		// Click outside the floating popup backdrop dismisses it cleanly!
		if msg.X < r.x || msg.X >= r.x+r.w || msg.Y < r.y || msg.Y >= r.y+r.h {
			m.detail.open = false
			return m
		}
		// Click [ × Esc ] on modal top border row.
		if msg.Y == r.y && msg.X >= m.detail.closeZone.x1 && msg.X < m.detail.closeZone.x2 {
			m.detail.open = false
			return m
		}
		// Click action buttons on modal bottom action bar.
		if msg.Y == r.y+r.h-2 {
			for _, z := range m.detail.actionZones {
				if msg.X >= z.x1 && msg.X < z.x2 {
					m.detail.btnIdx = z.idx
					m.triggerDetailAction(z.idx)
					return m
				}
			}
		}
		return m
	}

	// Toolbar click (row 1).
	if msg.Y == 1 {
		for _, z := range m.tabZones {
			if msg.X >= z.x1 && msg.X < z.x2 {
				m.focus = focusToolbar
				m.activateToolbarControl(z.idx)
				return m
			}
		}
		return m
	}

	// Feed body click.
	if msg.Y >= m.bodyTopY && msg.Y < m.bodyTopY+m.bodyHeight {
		visIdx := m.scroll + (msg.Y - m.bodyTopY)
		if visIdx >= 0 && visIdx < len(m.rowTargets) {
			rt := m.rowTargets[visIdx]
			if rt.nodeID == "" {
				return m
			}
			now := time.Now()
			isDouble := rt.nodeID == m.lastClickID && now.Sub(m.lastClickTime) < 400*time.Millisecond
			m.lastClickID = rt.nodeID
			m.lastClickTime = now

			m.focus = focusFeed
			m.selectedID = rt.nodeID
			m.follow = false

			switch {
			case rt.action == "inspect" || isDouble:
				m.openDetailForNode(rt.nodeID)
			case rt.action == "toggle":
				m.toggleNode(rt.nodeID)
				m.syncFeedLayout()
			}
		}
	}
	return m
}

func (m *Model) ingestEvent(ev events.Event) {
	if ev.SessionID != "" {
		m.recordSession(ev.SessionID)
	}
	hadNodes := len(m.nodeOrder) > 0

	switch ev.Kind {
	case events.KindExecStart:
		m.totalRuns++
		m.activeRuns++
		g := &runGroup{
			id:       "grp:" + ev.RunID,
			runID:    ev.RunID,
			isExec:   true,
			startEv:  ev,
			expanded: true,
		}
		if ev.Detail != "" {
			g.children = append(g.children, &childItem{
				id:       fmt.Sprintf("code:%s", ev.RunID),
				ev:       events.Event{Kind: events.KindExecStart, RunID: ev.RunID, SessionID: ev.SessionID, Cwd: ev.Cwd, Tool: "code", Title: "TypeScript source", Summary: countLinesLabel(ev.Detail), Detail: ev.Detail, DetailKind: "code", Truncated: ev.Truncated, Timestamp: ev.Timestamp},
				expanded: false,
			})
		}
		m.groups = append(m.groups, g)
		m.groupByRun[ev.RunID] = g
		m.trimGroups()

	case events.KindExecEnd:
		if m.activeRuns > 0 {
			m.activeRuns--
		}
		if g, ok := m.groupByRun[ev.RunID]; ok {
			evCopy := ev
			g.endEv = &evCopy
			if ev.Detail != "" || ev.Error != "" {
				body := ev.Detail
				if body == "" {
					body = ev.Error
				}
				g.children = append(g.children, &childItem{
					id: fmt.Sprintf("res:%s:%d", ev.RunID, ev.Seq),
					ev: events.Event{
						Kind:       events.KindExecEnd,
						RunID:      ev.RunID,
						SessionID:  ev.SessionID,
						Cwd:        g.startEv.Cwd,
						Tool:       "result",
						Title:      "Output / Return",
						Status:     ev.Status,
						Summary:    countLinesLabel(body),
						Error:      ev.Error,
						Detail:     body,
						DetailKind: "text",
						Truncated:  ev.Truncated,
						Timestamp:  ev.Timestamp,
					},
					expanded: ev.Status != "ok",
				})
			}
		}

	case events.KindTxStatus:
		if g, ok := m.groupByRun[ev.RunID]; ok {
			for _, ch := range g.children {
				if ch.ev.TxID == ev.TxID && ev.TxID != "" {
					ch.ev.TxPhase = ev.TxPhase
				}
			}
			ev.Cwd = g.startEv.Cwd
			g.children = append(g.children, &childItem{
				id:       fmt.Sprintf("ev:%d", ev.Seq),
				ev:       ev,
				expanded: ev.TxPhase == "rolled_back",
			})
		}

	case events.KindStreamChunk:
		sr, ok := m.streamByID[ev.StreamID]
		if !ok || sr == nil {
			return
		}
		target := sr.eventPtr()
		var capped bool
		target.Detail, capped = events.CapTailPreview(target.Detail+ev.Detail, events.DefaultShellPreview)
		if capped {
			target.Truncated = true
		}
		if ev.Summary != "" {
			target.Summary = ev.Summary
		}
		m.refreshOpenDetailForStream(*target)
		m.syncFeedLayout()
		if m.feedQuery != "" {
			m.rebuildFeedMatches()
		}
		if m.follow {
			m.scroll = max(0, m.totalVisRows-m.bodyHeight)
		}
		return

	default:
		// In-place update when a running shell/task stream transitions to its terminal state.
		if ev.StreamID != "" {
			if sr, exists := m.streamByID[ev.StreamID]; exists && sr != nil {
				target := sr.eventPtr()
				if ev.Cwd == "" {
					ev.Cwd = target.Cwd
				}
				if ev.Detail == "" && ev.Status == "running" {
					ev.Detail = target.Detail
				}
				*target = ev
				if sr.countedActive && ev.Status != "running" {
					sr.countedActive = false
					if m.activeRuns > 0 {
						m.activeRuns--
					}
				}
				m.refreshOpenDetailForStream(*target)
				break
			}
		}

		isWriteCRUD := ev.Kind == events.KindFileCreate || ev.Kind == events.KindFileUpdate ||
			ev.Kind == events.KindFileDelete || ev.Kind == events.KindFileRename
		if isWriteCRUD {
			m.totalCRUD++
		}
		if ev.Kind == events.KindTask {
			m.totalTasks++
		}
		if ev.Diff != nil {
			m.totalAdd += ev.Diff.Added
			m.totalDel += ev.Diff.Removed
		}

		if g, ok := m.groupByRun[ev.RunID]; ok && g.isExec {
			if ev.Cwd == "" {
				ev.Cwd = g.startEv.Cwd
			}
			if ev.Diff != nil {
				g.addedSum += ev.Diff.Added
				g.removedSum += ev.Diff.Removed
			}
			ch := &childItem{
				id:       fmt.Sprintf("ev:%d", ev.Seq),
				ev:       ev,
				expanded: isWriteCRUD || ev.Status == "running" || ev.Status == "error" || ev.Status == "denied",
			}
			g.children = append(g.children, ch)
			if ev.StreamID != "" {
				m.streamByID[ev.StreamID] = &streamRef{nodeID: ch.id, group: g, child: ch}
			}
		} else {
			g := &runGroup{
				id:       fmt.Sprintf("solo:%d", ev.Seq),
				runID:    ev.RunID,
				isExec:   false,
				startEv:  ev,
				expanded: isWriteCRUD || ev.Status == "running" || ev.Status == "error",
			}
			if ev.Diff != nil {
				g.addedSum = ev.Diff.Added
				g.removedSum = ev.Diff.Removed
			}
			m.groups = append(m.groups, g)
			if ev.StreamID != "" {
				sr := &streamRef{nodeID: g.id, group: g}
				if ev.Status == "running" {
					m.activeRuns++
					sr.countedActive = true
				}
				m.streamByID[ev.StreamID] = sr
			}
			m.trimGroups()
		}
	}

	m.syncFeedLayout()
	if m.feedQuery != "" {
		m.rebuildFeedMatches()
	}
	if len(m.nodeOrder) > 0 {
		if !hadNodes && m.follow {
			m.focus = focusFeed
		}
		if m.follow || m.selectedID == "" {
			m.selectedID = m.nodeOrder[len(m.nodeOrder)-1]
		}
	}
	if m.follow {
		m.scroll = max(0, m.totalVisRows-m.bodyHeight)
	}
}

func (m *Model) trimGroups() {
	const maxGroups = 250
	if len(m.groups) > maxGroups {
		drop := m.groups[0]
		delete(m.groupByRun, drop.runID)
		for sid, sr := range m.streamByID {
			if sr.group == drop {
				if sr.countedActive && m.activeRuns > 0 {
					m.activeRuns--
				}
				delete(m.streamByID, sid)
			}
		}
		m.groups = m.groups[1:]
	}
}

func (m *Model) recordSession(sid string) {
	for _, s := range m.sessionList {
		if s == sid {
			return
		}
	}
	m.sessionList = append(m.sessionList, sid)
}

func (m *Model) activeSessionFilter() string {
	if m.sessionIdx <= 0 || m.sessionIdx > len(m.sessionList) {
		return ""
	}
	return m.sessionList[m.sessionIdx-1]
}

func (m *Model) clearFeed() {
	for _, sr := range m.streamByID {
		if sr.countedActive && m.activeRuns > 0 {
			m.activeRuns--
		}
	}
	m.groups = nil
	m.groupByRun = map[string]*runGroup{}
	m.streamByID = map[string]*streamRef{}
	m.nodeOrder = nil
	m.rowTargets = nil
	m.totalVisRows = 0
	m.selectedID = ""
	m.scroll = 0
	m.feedMatches = nil
	m.focus = focusToolbar
}

func (m *Model) toggleNode(id string) {
	for _, g := range m.groups {
		if g.id == id {
			g.expanded = !g.expanded
			return
		}
		for _, ch := range g.children {
			if ch.id == id {
				ch.expanded = !ch.expanded
				return
			}
		}
	}
}

func (m *Model) setAllExpanded(on bool) {
	for _, g := range m.groups {
		g.expanded = on
		for _, ch := range g.children {
			ch.expanded = on
		}
	}
}

func (m *Model) syncFeedLayout() {
	rows, targets := m.buildFeedRows()
	m.rowTargets = targets
	m.totalVisRows = len(rows)
}

func (m *Model) snapSelectionToVisible() {
	if len(m.nodeOrder) == 0 {
		m.selectedID = ""
		m.scroll = 0
		m.focus = focusToolbar
		return
	}
	for _, id := range m.nodeOrder {
		if id == m.selectedID {
			m.ensureSelectedVisible()
			return
		}
	}
	if m.follow {
		m.selectedID = m.nodeOrder[len(m.nodeOrder)-1]
		m.scrollToBottom()
	} else {
		m.selectedID = m.nodeOrder[0]
		m.scroll = 0
	}
}

func (m *Model) collapseOrParent() {
	m.syncFeedLayout()
	if m.selectedID == "" {
		m.focus = focusToolbar
		return
	}
	for _, g := range m.groups {
		if g.id == m.selectedID {
			if g.expanded {
				g.expanded = false
				m.syncFeedLayout()
				m.ensureSelectedVisible()
			} else {
				m.focus = focusToolbar
			}
			return
		}
		for _, ch := range g.children {
			if ch.id == m.selectedID {
				if ch.expanded {
					ch.expanded = false
					m.syncFeedLayout()
					m.ensureSelectedVisible()
				} else {
					m.selectedID = g.id
					m.ensureSelectedVisible()
				}
				return
			}
		}
	}
}

func (m *Model) expandOrInspect() {
	m.syncFeedLayout()
	if m.selectedID == "" {
		return
	}
	for _, g := range m.groups {
		if g.id == m.selectedID {
			if !g.expanded {
				g.expanded = true
				m.syncFeedLayout()
				m.ensureSelectedVisible()
				return
			}
			if g.isExec {
				for _, ch := range g.children {
					if m.eventMatchesTab(ch.ev) {
						m.selectedID = ch.id
						m.ensureSelectedVisible()
						return
					}
				}
			}
			m.openDetailForNode(g.id)
			return
		}
		for _, ch := range g.children {
			if ch.id == m.selectedID {
				if !ch.expanded {
					ch.expanded = true
					m.syncFeedLayout()
					m.ensureSelectedVisible()
				} else {
					m.openDetailForNode(ch.id)
				}
				return
			}
		}
	}
}

func (m *Model) moveSelection(delta int) {
	m.syncFeedLayout()
	if len(m.nodeOrder) == 0 {
		m.focus = focusToolbar
		return
	}
	idx := -1
	for i, id := range m.nodeOrder {
		if id == m.selectedID {
			idx = i
			break
		}
	}
	if idx < 0 {
		if delta > 0 {
			idx = 0
		} else {
			idx = len(m.nodeOrder) - 1
		}
	} else {
		if delta < 0 && idx == 0 {
			m.focus = focusToolbar
			return
		}
		idx = max(0, min(len(m.nodeOrder)-1, idx+delta))
	}
	m.selectedID = m.nodeOrder[idx]
	m.ensureSelectedVisible()
	maxScroll := max(0, m.totalVisRows-m.bodyHeight)
	if delta > 0 && idx == len(m.nodeOrder)-1 && m.scroll >= maxScroll {
		m.follow = true
	}
}

func (m *Model) ensureSelectedVisible() {
	if m.selectedID == "" {
		return
	}
	m.syncFeedLayout()
	for i, rt := range m.rowTargets {
		if rt.nodeID == m.selectedID && (rt.action == "toggle" || rt.action == "inspect") {
			if i < m.scroll {
				m.scroll = i
			} else if i >= m.scroll+m.bodyHeight {
				m.scroll = max(0, i-m.bodyHeight+2)
			}
			maxScroll := max(0, m.totalVisRows-m.bodyHeight)
			if m.scroll > maxScroll {
				m.scroll = maxScroll
			}
			return
		}
	}
}

func (m *Model) scrollFeed(delta int) {
	m.syncFeedLayout()
	maxScroll := max(0, m.totalVisRows-m.bodyHeight)
	m.scroll = max(0, min(maxScroll, m.scroll+delta))
	if m.scroll >= maxScroll && delta > 0 {
		m.follow = true
	}
}

func (m *Model) scrollToBottom() {
	m.syncFeedLayout()
	maxScroll := max(0, m.totalVisRows-m.bodyHeight)
	m.scroll = maxScroll
}

func (m *Model) rebuildFeedMatches() {
	m.feedMatches = nil
	q := strings.ToLower(strings.TrimSpace(m.feedQuery))
	if q == "" {
		return
	}
	for _, g := range m.groups {
		if !m.groupMatchesTab(g) {
			continue
		}
		if groupTextContains(g, q) {
			m.feedMatches = append(m.feedMatches, g.id)
		}
		for _, ch := range g.children {
			if m.eventMatchesTab(ch.ev) && eventTextContains(ch.ev, q) {
				m.feedMatches = append(m.feedMatches, ch.id)
			}
		}
	}
}

func (m *Model) jumpFeedMatch(step int) {
	if len(m.feedMatches) == 0 {
		return
	}
	m.feedMatchAt = (m.feedMatchAt + step + len(m.feedMatches)) % len(m.feedMatches)
	m.selectedID = m.feedMatches[m.feedMatchAt]
	m.focus = focusFeed
	m.follow = false
	for _, g := range m.groups {
		for _, ch := range g.children {
			if ch.id == m.selectedID {
				g.expanded = true
				break
			}
		}
	}
	m.ensureSelectedVisible()
}

func (m *Model) openDetailForNode(id string) {
	for _, g := range m.groups {
		if g.id == id {
			m.openDetailForEvent(g.startEv, g.endEv)
			return
		}
		for _, ch := range g.children {
			if ch.id == id {
				m.openDetailForEvent(ch.ev, nil)
				return
			}
		}
	}
}

func buildDetailContent(ev events.Event, endEv *events.Event) (badge, absPath, metaLine string, lines []detailLine) {
	badge = ev.Summary
	if ev.Diff != nil && (ev.Diff.Added > 0 || ev.Diff.Removed > 0) {
		badge = fmt.Sprintf("+%d -%d", ev.Diff.Added, ev.Diff.Removed)
	}
	if ev.TxID != "" {
		badge += fmt.Sprintf("  [%s %s]", ev.TxID, ev.TxPhase)
	}

	absPath = ev.AbsPath
	if absPath == "" && ev.Cwd != "" && ev.Path != "" {
		absPath = strings.TrimRight(ev.Cwd, "/") + "/" + strings.TrimLeft(ev.Path, "/")
	}
	if absPath == "" {
		absPath = ev.Cwd
	}

	var metaParts []string
	if ev.Tool != "" {
		metaParts = append(metaParts, "Tool: "+ev.Tool)
	}
	if ev.Cwd != "" {
		metaParts = append(metaParts, "Cwd: "+ev.Cwd)
	}
	if ev.Status == "running" {
		metaParts = append(metaParts, "Status: running (live)")
	} else if ev.DurationMs > 0 {
		metaParts = append(metaParts, fmt.Sprintf("%dms", ev.DurationMs))
	}
	if ev.Truncated {
		metaParts = append(metaParts, "preview capped")
	}
	metaLine = strings.Join(metaParts, "  ·  ")

	if ev.Diff != nil && len(ev.Diff.Hunks) > 0 {
		lines = renderDiffDetailLines(ev.Diff)
		return strings.TrimSpace(badge), absPath, metaLine, lines
	}

	body := ev.Detail
	if endEv != nil && endEv.Detail != "" {
		if body != "" {
			body += "\n\n--- Execution Result ---\n" + endEv.Detail
		} else {
			body = endEv.Detail
		}
	}
	if body == "" && ev.Error != "" {
		body = ev.Error
	}
	if body == "" && ev.OldPath != "" {
		from := ev.OldAbsPath
		if from == "" {
			from = ev.OldPath
		}
		to := absPath
		if to == "" {
			to = ev.Path
		}
		body = fmt.Sprintf("From: %s\nTo:   %s", from, to)
	}
	if body == "" {
		if ev.Status == "running" {
			body = "(running — waiting for output…)"
		} else {
			body = "(no detailed payload recorded)"
		}
	}
	rawLines := strings.Split(strings.ReplaceAll(strings.TrimRight(body, "\n"), "\r\n", "\n"), "\n")
	for i, l := range rawLines {
		gutter := styleLineNum.Render(fmt.Sprintf("%5d │ ", i+1))
		lines = append(lines, detailLine{
			gutter: gutter,
			plain:  l,
			styled: gutter + styleDiffCtx.Render(l),
		})
	}
	return strings.TrimSpace(badge), absPath, metaLine, lines
}

func (m *Model) openDetailForEvent(ev events.Event, endEv *events.Event) {
	title := formatOpCallTitle(ev)
	badge, absPath, metaLine, lines := buildDetailContent(ev, endEv)
	isStreaming := ev.Status == "running" && ev.StreamID != ""
	initScroll := 0
	if isStreaming {
		initScroll = max(0, len(lines)-m.modalPageHeight())
	}

	m.detail = detailState{
		open:       true,
		streamID:   ev.StreamID,
		streaming:  isStreaming,
		followTail: isStreaming,
		title:      title,
		badgeText:  badge,
		absPath:    absPath,
		oldAbsPath: ev.OldAbsPath,
		cwd:        ev.Cwd,
		metaLine:   metaLine,
		lines:      lines,
		scroll:     initScroll,
		btnIdx:     0,
		hoverBtn:   -1,
	}
}

func (m *Model) refreshOpenDetailForStream(ev events.Event) {
	if !m.detail.open || m.detail.streamID == "" || m.detail.streamID != ev.StreamID {
		return
	}
	badge, absPath, metaLine, lines := buildDetailContent(ev, nil)
	m.detail.badgeText = badge
	if absPath != "" {
		m.detail.absPath = absPath
	}
	m.detail.metaLine = metaLine
	m.detail.lines = lines
	m.detail.streaming = ev.Status == "running"
	if m.detail.query != "" {
		m.recomputeDetailMatches()
	}
	page := m.modalPageHeight()
	maxScroll := max(0, len(m.detail.lines)-page)
	if m.detail.followTail {
		m.detail.scroll = maxScroll
	} else if m.detail.scroll > maxScroll {
		m.detail.scroll = maxScroll
	}
}

func renderDiffDetailLines(d *diff.Result) []detailLine {
	var out []detailLine
	for hi, h := range d.Hunks {
		if hi > 0 {
			sep := fmt.Sprintf("  ⋯   │  @@ -%d,%d +%d,%d @@", h.OldStart, h.OldCount, h.NewStart, h.NewCount)
			out = append(out, detailLine{plain: sep, styled: styleHunkSep.Render(sep)})
		}
		for _, dl := range h.Lines {
			bgHex := ""
			switch dl.Kind {
			case diff.LineAdd:
				bgHex = colorDiffAddBg
			case diff.LineDel:
				bgHex = colorDiffDelBg
			}
			out = append(out, detailLine{
				gutter: formatDiffGutter(dl),
				plain:  dl.Text,
				styled: renderColoredDiffLine(dl, 0),
				bgHex:  bgHex,
			})
		}
	}
	if d.Truncated {
		note := fmt.Sprintf("  ⋯   │  ... diff truncated (total +%d -%d lines)", d.Added, d.Removed)
		out = append(out, detailLine{plain: note, styled: styleHunkSep.Render(note)})
	}
	return out
}

func (m *Model) scrollDetail(delta int) {
	page := m.modalPageHeight()
	maxScroll := max(0, len(m.detail.lines)-page)
	m.detail.scroll = max(0, min(maxScroll, m.detail.scroll+delta))
	if delta < 0 {
		m.detail.followTail = false
	} else if m.detail.scroll >= maxScroll {
		m.detail.followTail = true
	}
}

func compileSearchRegex(q string) *regexp.Regexp {
	q = strings.TrimSpace(q)
	if q == "" {
		return nil
	}
	if re, err := regexp.Compile("(?i)" + q); err == nil {
		return re
	}
	re, _ := regexp.Compile("(?i)" + regexp.QuoteMeta(q))
	return re
}

func (m *Model) recomputeDetailMatches() {
	m.detail.matches = nil
	re := compileSearchRegex(m.detail.query)
	if re == nil {
		return
	}
	for i, dl := range m.detail.lines {
		if re.MatchString(dl.plain) {
			m.detail.matches = append(m.detail.matches, i)
		}
	}
}

func (m *Model) stepDetailMatch(delta int) {
	if len(m.detail.matches) == 0 {
		return
	}
	m.detail.matchCursor = (m.detail.matchCursor + delta + len(m.detail.matches)) % len(m.detail.matches)
	m.jumpDetailToMatch(m.detail.matchCursor)
}

func (m *Model) jumpDetailToMatch(cursor int) {
	if cursor < 0 || cursor >= len(m.detail.matches) {
		return
	}
	lineIdx := m.detail.matches[cursor]
	page := m.modalPageHeight()
	targetScroll := max(0, lineIdx-page/3)
	maxScroll := max(0, len(m.detail.lines)-page)
	m.detail.scroll = min(maxScroll, targetScroll)
	m.detail.followTail = false
}

func (m *Model) groupMatchesTab(g *runGroup) bool {
	if sf := m.activeSessionFilter(); sf != "" && g.startEv.SessionID != sf {
		return false
	}
	if m.activeTab == tabAll {
		return true
	}
	if !g.isExec {
		return m.eventMatchesTab(g.startEv)
	}
	for _, ch := range g.children {
		if m.eventMatchesTab(ch.ev) {
			return true
		}
	}
	return false
}

func (m *Model) eventMatchesTab(ev events.Event) bool {
	switch m.activeTab {
	case tabCRUD:
		return ev.Kind == events.KindFileCreate || ev.Kind == events.KindFileUpdate ||
			ev.Kind == events.KindFileDelete || ev.Kind == events.KindFileRename ||
			ev.Kind == events.KindTxStatus
	case tabReadSearch:
		return ev.Kind == events.KindFileRead || ev.Kind == events.KindSearch
	case tabShellTask:
		return ev.Kind == events.KindShell || ev.Kind == events.KindTask
	default:
		return true
	}
}

func groupTextContains(g *runGroup, lowQ string) bool {
	if eventTextContains(g.startEv, lowQ) {
		return true
	}
	if g.endEv != nil && eventTextContains(*g.endEv, lowQ) {
		return true
	}
	return false
}

func eventTextContains(ev events.Event, lowQ string) bool {
	for _, s := range []string{ev.Path, ev.AbsPath, ev.OldPath, ev.Title, ev.Tool, ev.Summary, ev.Error, ev.Detail} {
		if s != "" && strings.Contains(strings.ToLower(s), lowQ) {
			return true
		}
	}
	if ev.Diff != nil {
		for _, h := range ev.Diff.Hunks {
			for _, l := range h.Lines {
				if strings.Contains(strings.ToLower(l.Text), lowQ) {
					return true
				}
			}
		}
	}
	return false
}

func countLinesLabel(s string) string {
	if s == "" {
		return "0 lines"
	}
	n := strings.Count(s, "\n") + 1
	return fmt.Sprintf("%d lines · %dB", n, len(s))
}

func clampLineWidth(s string, maxWidth int) string {
	if maxWidth <= 0 {
		return s
	}
	if ansi.StringWidth(s) <= maxWidth {
		return s
	}
	if maxWidth <= 1 {
		return ansi.Truncate(s, maxWidth, "")
	}
	return ansi.Truncate(s, maxWidth, "…")
}
