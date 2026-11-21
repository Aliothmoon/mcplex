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
	copyX1 int    // clickable inline card header [ Copy ] button start X (exclusive end copyX2)
	copyX2 int
}

type modalCopyZone struct {
	y      int
	x1, x2 int
	group  int // panGroup to copy (1=CODE, 2=OUTPUT)
}

type cardRenderCache struct {
	valid     bool
	feedW     int
	isLast    bool
	hoverCopy bool
	hScroll   int
	painted   []string
	plains    []string
	moreCount int
	copyX1    int
	copyX2    int
}

type groupOpsCache struct {
	valid   bool
	feedW   int
	noChild bool
	painted string
	plain   string
}

type feedCacheKey struct {
	width           int
	activeTab       filterTab
	activeSessionID string
	multiSession    bool
	feedFocused     bool
	selectedID      string
	hoverNodeID     string
	hoverCopyNodeID string
	spinnerFrame    int
	feedRev         uint64
	structureHash   uint64
}

type childItem struct {
	id       string
	ev       events.Event
	expanded bool
	hScroll  int
	cache    cardRenderCache
}

type runGroup struct {
	id         string
	runID      string
	isExec     bool
	startEv    events.Event
	endEv      *events.Event
	children   []*childItem
	expanded   bool
	hScroll    int
	addedSum   int
	removedSum int
	bodyCache  cardRenderCache
	opsCache   groupOpsCache
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
	gutter         string // pinned styled gutter (e.g. "   12 │ "), empty for Ops Flow / hunk header
	plain          string // plain body text without gutter (for search & horizontal offset math)
	body           string // styled body text without gutter (for pinned-gutter horizontal pan & soft-wrap)
	bgHex          string
	panGroup       int // >0: lines sharing the same panGroup horizontally scroll together; 0: stationary
	headerForGroup int // >0: stationary section header representing panGroup (shows ◂ +N badge & routes ←→)
}

type modalFocusArea int

const (
	modalFocusBody modalFocusArea = iota
	modalFocusBar
)

type modalRect struct {
	x, y, w, h int
}

type sessionInfo struct {
	id       string
	label    string
	cwd      string
	runs     int
	crud     int
	tasks    int
	active   int
	lastTs   string
	colorIdx int
}

type pickerRowZone struct {
	y   int
	idx int
}

type sessionPickerState struct {
	open      bool
	cursor    int // 0 = All Sessions, 1..len(sessions) = specific session
	hoverIdx  int // -1 = none, -2 = close chip
	rect      modalRect
	closeZone hitZone
	rowZones  []pickerRowZone
}

type detailState struct {
	open           bool
	focus          modalFocusArea
	runID          string
	isExecGroup    bool
	streamID       string
	streaming      bool
	followTail     bool
	wrap           bool
	title          string
	badgeText      string
	absPath        string
	oldAbsPath     string
	cwd            string
	metaLine       string
	lines          []detailLine
	scroll         int
	cursor         int         // active visual row index inside modal body
	groupHScroll   map[int]int // per-panGroup horizontal scroll offset in display columns (when wrap == false)
	totalVis       int         // cached total visual rows for current wrap/width
	searchMode     bool
	searchInput    string
	query          string
	matches        []int
	matchCursor    int
	btnIdx         int // focused button index in popup bottom action bar (0..5)
	hoverBtn       int // hovered button index (-1=none, -2=close chip)
	hoverRow       int // hovered visual body row index (-1=none)
	rect           modalRect
	closeZone      hitZone         // clickable [ esc × ] on modal top border row
	actionZones    []hitZone       // clickable buttons on modal bottom action row
	copyZones      []modalCopyZone // clickable [ Copy ] buttons on CODE/OUTPUT section headers
	hoverCopyGroup int             // hovered section copy group (0=none)
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

	groups          []*runGroup
	groupByRun      map[string]*runGroup
	streamByID      map[string]*streamRef
	nodeOrder       []string
	selectedID      string
	scroll          int
	follow          bool
	activeTab       filterTab
	sessions        []*sessionInfo
	sessionByID     map[string]*sessionInfo
	activeSessionID string
	sessPicker      sessionPickerState

	// Feed search state (/ + n / N)
	searchMode  bool
	searchInput string
	feedQuery   string
	feedMatches []string
	feedMatchAt int

	// Cached layout & mouse hit-testing / hover state + feed render cache
	feedRows        []string
	feedDirty       bool
	feedRev         uint64
	lastCacheKey    feedCacheKey
	tabZones        []hitZone
	rowTargets      []rowTarget
	lastFeedPlain   []string
	totalVisRows    int
	bodyTopY        int
	bodyHeight      int
	hoverToolbar    int
	hoverNodeID     string
	hoverCopyNodeID string

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

	// Floating overlay detail popup (less-style cursor + scroll + h-pan + wrap + search)
	detail     detailState
	detailWrap bool // persists user's word-wrap preference across popups

	// Mouse drag text selection + select-to-copy (选中即复制) + visual mode & toast
	sel          textSelection
	copyToast    string
	copyToastErr bool
	copyToastSeq int

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
		sessionByID:  map[string]*sessionInfo{},
		sessPicker:   sessionPickerState{hoverIdx: -1},
		follow:       true,
		connected:    cfg.ModeLabel == "local",
		feedDirty:    true,
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
	if len(m.sessions) > 0 {
		label := fmt.Sprintf("Sess:All(%d)", len(m.sessions))
		if sf := m.activeSessionFilter(); sf != "" {
			name := m.sessionDisplayName(sf)
			if len([]rune(name)) > 14 {
				name = string([]rune(name)[:13]) + "…"
			}
			label = "Sess:" + name
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

	case copyToastClearMsg:
		if msg.seq == m.copyToastSeq {
			m.copyToast = ""
			m.copyToastErr = false
		}
		return m, nil

	case tea.MouseMsg:
		return m.handleMouse(msg)

	case tea.KeyMsg:
		return m.handleKey(msg)
	}
	return m, nil
}

func (m *Model) recomputeLayoutMetrics() {
	m.bodyTopY = 3
	m.bodyHeight = max(1, m.height-4)
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
	if m.sessPicker.open {
		return m.handleSessionPickerKey(msg)
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
	case "s":
		m.openSessionPicker()
		return m, nil
	case "n":
		m.jumpFeedMatch(1)
		return m, nil
	case "N":
		m.jumpFeedMatch(-1)
		return m, nil
	case "y":
		return m, m.copyFromFeed()
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
		if m.sel.active && m.sel.scope == selScopeFeed {
			m.clearSelection()
			return m, nil
		}
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
		m.openSessionPicker()
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

func (m *Model) openSessionPicker() {
	cursor := 0
	if m.activeSessionID != "" {
		for i, s := range m.sessions {
			if s.id == m.activeSessionID {
				cursor = i + 1
				break
			}
		}
	}
	m.sessPicker = sessionPickerState{
		open:     true,
		cursor:   cursor,
		hoverIdx: -1,
	}
}

func (m *Model) applySessionPickerChoice(idx int) {
	if idx <= 0 || idx > len(m.sessions) {
		m.activeSessionID = ""
	} else {
		m.activeSessionID = m.sessions[idx-1].id
	}
	m.sessPicker.open = false
	m.syncFeedLayout()
	m.snapSelectionToVisible()
	m.rebuildFeedMatches()
}

func (m *Model) handleSessionPickerKey(msg tea.KeyMsg) (tea.Model, tea.Cmd) {
	total := len(m.sessions) + 1 // 0 = All Sessions, 1..N = individual sessions
	switch msg.Type {
	case tea.KeyEsc:
		m.sessPicker.open = false
		return m, nil
	case tea.KeyUp:
		m.sessPicker.cursor = (m.sessPicker.cursor - 1 + total) % total
		return m, nil
	case tea.KeyDown:
		m.sessPicker.cursor = (m.sessPicker.cursor + 1) % total
		return m, nil
	case tea.KeyHome:
		m.sessPicker.cursor = 0
		return m, nil
	case tea.KeyEnd:
		m.sessPicker.cursor = max(0, total-1)
		return m, nil
	case tea.KeyEnter:
		m.applySessionPickerChoice(m.sessPicker.cursor)
		return m, nil
	}
	switch msg.String() {
	case "q", "s":
		m.sessPicker.open = false
	case "k":
		m.sessPicker.cursor = (m.sessPicker.cursor - 1 + total) % total
	case "j":
		m.sessPicker.cursor = (m.sessPicker.cursor + 1) % total
	case " ":
		m.applySessionPickerChoice(m.sessPicker.cursor)
	case "0":
		m.applySessionPickerChoice(0)
	case "1", "2", "3", "4", "5", "6", "7", "8", "9":
		n := int(msg.String()[0] - '0')
		if n < total {
			m.applySessionPickerChoice(n)
		}
	}
	return m, nil
}

func (m *Model) cycleSession() {
	m.openSessionPicker()
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
	case tea.KeySpace:
		m.searchInput += " "
		return m, nil
	case tea.KeyRunes:
		m.searchInput += sanitizeSearchPasteRunes(msg.Runes)
		return m, nil
	}
	if msg.String() == " " {
		m.searchInput += " "
	}
	return m, nil
}

func sanitizeSearchPasteRunes(runes []rune) string {
	if len(runes) == 0 {
		return ""
	}
	return strings.NewReplacer("\r\n", " ", "\n", " ", "\r", " ").Replace(string(runes))
}

const modalActionCount = 7 // Search, Prev, Next, Wrap, Copy, Top, End

func (m *Model) triggerDetailAction(idx int) tea.Cmd {
	switch idx {
	case 0: // / Search
		m.detail.searchMode = true
		m.detail.searchInput = m.detail.query
	case 1: // Prev match
		m.stepDetailMatch(-1)
	case 2: // Next match
		m.stepDetailMatch(1)
	case 3: // Wrap toggle
		m.toggleDetailWrap()
	case 4: // Copy active selection or section
		return m.copyFromModal(false)
	case 5: // Top
		m.jumpDetailTop()
	case 6: // End
		m.jumpDetailBottom()
	}
	return nil
}

func (m *Model) updateModalQueryFromInput() {
	if strings.TrimSpace(m.detail.searchInput) == "" {
		m.detail.query = ""
	} else {
		m.detail.query = m.detail.searchInput
	}
	m.recomputeDetailMatches()
}

func (m *Model) handleModalSearchKey(msg tea.KeyMsg) (tea.Model, tea.Cmd) {
	switch msg.Type {
	case tea.KeyEsc:
		m.detail.searchMode = false
		return m, nil
	case tea.KeyEnter:
		m.detail.searchMode = false
		m.updateModalQueryFromInput()
		if len(m.detail.matches) > 0 {
			m.detail.matchCursor = 0
			m.jumpDetailToMatch(0)
		}
		m.detail.focus = modalFocusBody
		return m, nil
	case tea.KeyBackspace:
		r := []rune(m.detail.searchInput)
		if len(r) > 0 {
			m.detail.searchInput = string(r[:len(r)-1])
		}
		m.updateModalQueryFromInput()
		return m, nil
	case tea.KeySpace:
		m.detail.searchInput += " "
		m.updateModalQueryFromInput()
		return m, nil
	case tea.KeyRunes:
		m.detail.searchInput += sanitizeSearchPasteRunes(msg.Runes)
		m.updateModalQueryFromInput()
		return m, nil
	}
	if msg.String() == " " {
		m.detail.searchInput += " "
		m.updateModalQueryFromInput()
	}
	return m, nil
}

func (m *Model) handleDetailKey(msg tea.KeyMsg) (tea.Model, tea.Cmd) {
	if m.detail.searchMode {
		return m.handleModalSearchKey(msg)
	}

	page := m.modalPageHeight()
	// Global modal keys regardless of body/bar focus
	switch msg.Type {
	case tea.KeyEsc:
		if m.sel.active && m.sel.scope == selScopeModal {
			m.clearSelection()
		} else if m.detail.query != "" {
			m.detail.query = ""
			m.detail.searchInput = ""
			m.detail.matches = nil
		} else {
			m.clearSelection()
			m.detail.open = false
		}
		return m, nil
	case tea.KeyTab, tea.KeyShiftTab:
		if m.detail.focus == modalFocusBody {
			m.detail.focus = modalFocusBar
		} else {
			m.detail.focus = modalFocusBody
		}
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
		m.jumpDetailTop()
		return m, nil
	case tea.KeyEnd:
		m.jumpDetailBottom()
		return m, nil
	}

	switch msg.String() {
	case "q", "o":
		m.clearSelection()
		m.detail.open = false
		return m, nil
	case "w":
		m.clearSelection()
		m.toggleDetailWrap()
		return m, nil
	case "v":
		m.toggleModalVisualSelect()
		return m, nil
	case "y":
		return m, m.copyFromModal(false)
	case "Y":
		return m, m.copyFromModal(true)
	case "/":
		m.detail.searchMode = true
		m.detail.searchInput = m.detail.query
		return m, nil
	case "n":
		m.stepDetailMatch(1)
		return m, nil
	case "N":
		m.stepDetailMatch(-1)
		return m, nil
	case "g":
		m.jumpDetailTop()
		return m, nil
	case "G":
		m.jumpDetailBottom()
		return m, nil
	case "b":
		m.scrollDetail(-page)
		return m, nil
	case "f":
		m.scrollDetail(page)
		return m, nil
	case "d":
		m.scrollDetail(max(2, page/2))
		return m, nil
	case "u":
		m.scrollDetail(-max(2, page/2))
		return m, nil
	}

	if m.detail.focus == modalFocusBar {
		switch msg.Type {
		case tea.KeyUp:
			m.detail.focus = modalFocusBody
			return m, nil
		case tea.KeyLeft:
			m.detail.btnIdx = (m.detail.btnIdx - 1 + modalActionCount) % modalActionCount
			return m, nil
		case tea.KeyRight:
			m.detail.btnIdx = (m.detail.btnIdx + 1) % modalActionCount
			return m, nil
		case tea.KeyEnter:
			return m, m.triggerDetailAction(m.detail.btnIdx)
		}
		switch msg.String() {
		case "k":
			m.detail.focus = modalFocusBody
		case "h":
			m.detail.btnIdx = (m.detail.btnIdx - 1 + modalActionCount) % modalActionCount
		case "l":
			m.detail.btnIdx = (m.detail.btnIdx + 1) % modalActionCount
		case " ":
			return m, m.triggerDetailAction(m.detail.btnIdx)
		}
		return m, nil
	}

	// modalFocusBody: cursor up/down + horizontal pan left/right when wrap is off
	switch msg.Type {
	case tea.KeyDown:
		m.moveDetailCursor(1)
		return m, nil
	case tea.KeyUp:
		m.moveDetailCursor(-1)
		return m, nil
	case tea.KeyLeft:
		m.panDetailHorizontal(-8)
		return m, nil
	case tea.KeyRight:
		m.panDetailHorizontal(8)
		return m, nil
	case tea.KeyEnter:
		if m.sel.active && m.sel.scope == selScopeModal && m.sel.lineMode {
			return m, m.copyFromModal(false)
		}
		m.detail.focus = modalFocusBar
		return m, nil
	}
	switch msg.String() {
	case "j":
		m.moveDetailCursor(1)
	case "k":
		m.moveDetailCursor(-1)
	case "h":
		m.panDetailHorizontal(-8)
	case "l":
		m.panDetailHorizontal(8)
	case "0", "^":
		m.resetActiveDetailHScroll()
	case "$":
		m.panDetailToGroupEnd()
	case " ":
		m.scrollDetail(page)
	}
	return m, nil
}

func (m *Model) handleMouse(msg tea.MouseMsg) (tea.Model, tea.Cmd) {
	if m.sessPicker.open {
		r := m.sessPicker.rect
		if msg.Button == tea.MouseButtonWheelUp {
			total := len(m.sessions) + 1
			m.sessPicker.cursor = (m.sessPicker.cursor - 1 + total) % total
			return m, nil
		}
		if msg.Button == tea.MouseButtonWheelDown {
			total := len(m.sessions) + 1
			m.sessPicker.cursor = (m.sessPicker.cursor + 1) % total
			return m, nil
		}
		if msg.Action == tea.MouseActionMotion {
			m.sessPicker.hoverIdx = -1
			if msg.Y == r.y && msg.X >= m.sessPicker.closeZone.x1 && msg.X < m.sessPicker.closeZone.x2 {
				m.sessPicker.hoverIdx = -2
				return m, nil
			}
			for _, rz := range m.sessPicker.rowZones {
				if msg.Y == rz.y && msg.X > r.x && msg.X < r.x+r.w-1 {
					m.sessPicker.hoverIdx = rz.idx
					break
				}
			}
			return m, nil
		}
		if msg.Action == tea.MouseActionPress && msg.Button == tea.MouseButtonLeft {
			if msg.X < r.x || msg.X >= r.x+r.w || msg.Y < r.y || msg.Y >= r.y+r.h {
				m.sessPicker.open = false
				return m, nil
			}
			if msg.Y == r.y && msg.X >= m.sessPicker.closeZone.x1 && msg.X < m.sessPicker.closeZone.x2 {
				m.sessPicker.open = false
				return m, nil
			}
			for _, rz := range m.sessPicker.rowZones {
				if msg.Y == rz.y && msg.X > r.x && msg.X < r.x+r.w-1 {
					m.applySessionPickerChoice(rz.idx)
					return m, nil
				}
			}
		}
		return m, nil
	}

	if msg.Button == tea.MouseButtonWheelLeft {
		if m.detail.open {
			m.panDetailHoverOrActive(-6)
		}
		return m, nil
	}
	if msg.Button == tea.MouseButtonWheelRight {
		if m.detail.open {
			m.panDetailHoverOrActive(6)
		}
		return m, nil
	}
	if msg.Button == tea.MouseButtonWheelUp {
		if m.detail.open {
			if msg.Shift {
				m.panDetailHoverOrActive(-6)
			} else {
				m.scrollDetail(-3)
			}
		} else {
			m.follow = false
			m.scrollFeed(-3)
		}
		return m, nil
	}
	if msg.Button == tea.MouseButtonWheelDown {
		if m.detail.open {
			if msg.Shift {
				m.panDetailHoverOrActive(6)
			} else {
				m.scrollDetail(3)
			}
		} else {
			m.scrollFeed(3)
		}
		return m, nil
	}

	// Mouse button release -> finalize drag selection & auto-copy (选中即复制), or execute pending single-click!
	if msg.Action == tea.MouseActionRelease {
		if m.sel.dragging {
			return m, m.finishMouseDrag(msg.X, msg.Y)
		}
		return m, nil
	}

	if msg.Action == tea.MouseActionMotion {
		// Active left-button drag -> live-update text selection range!
		if m.sel.dragging {
			if msg.Button == tea.MouseButtonLeft {
				m.updateMouseDrag(msg.X, msg.Y)
				return m, nil
			}
			// Released outside terminal window -> finalize cleanly.
			return m, m.finishMouseDrag(msg.X, msg.Y)
		}

		if m.detail.open {
			m.detail.hoverBtn = -1
			m.detail.hoverRow = -1
			m.detail.hoverCopyGroup = 0
			r := m.detail.rect
			if msg.Y == r.y && msg.X >= m.detail.closeZone.x1 && msg.X < m.detail.closeZone.x2 {
				m.detail.hoverBtn = -2
				return m, nil
			}
			for _, cz := range m.detail.copyZones {
				if msg.Y == cz.y && msg.X >= cz.x1 && msg.X < cz.x2 {
					m.detail.hoverCopyGroup = cz.group
					return m, nil
				}
			}
			bodyStartY := r.y + 4
			bodyEndY := r.y + r.h - 3
			if msg.X > r.x && msg.X < r.x+r.w-1 && msg.Y >= bodyStartY && msg.Y < bodyEndY {
				visIdx := m.detail.scroll + (msg.Y - bodyStartY)
				if visIdx >= 0 && visIdx < m.detail.totalVis {
					m.detail.hoverRow = visIdx
				}
				return m, nil
			}
			if msg.Y == r.y+r.h-2 {
				for _, z := range m.detail.actionZones {
					if msg.X >= z.x1 && msg.X < z.x2 {
						m.detail.hoverBtn = z.idx
						break
					}
				}
			}
			return m, nil
		}
		m.hoverToolbar = -1
		m.hoverNodeID = ""
		m.hoverCopyNodeID = ""
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
				rt := m.rowTargets[visIdx]
				m.hoverNodeID = rt.nodeID
				if rt.copyX2 > rt.copyX1 && msg.X >= rt.copyX1 && msg.X < rt.copyX2 {
					m.hoverCopyNodeID = rt.nodeID
				}
			}
		}
		return m, nil
	}

	if msg.Action != tea.MouseActionPress || msg.Button != tea.MouseButtonLeft {
		return m, nil
	}

	if m.detail.open {
		r := m.detail.rect
		// Click outside the floating popup backdrop dismisses it cleanly!
		if msg.X < r.x || msg.X >= r.x+r.w || msg.Y < r.y || msg.Y >= r.y+r.h {
			m.clearSelection()
			m.detail.open = false
			return m, nil
		}
		// Click [ esc × ] on modal top border row.
		if msg.Y == r.y && msg.X >= m.detail.closeZone.x1 && msg.X < m.detail.closeZone.x2 {
			m.clearSelection()
			m.detail.open = false
			return m, nil
		}
		// Click a section header [ Copy ] button (CODE / OUTPUT) -> 1-click copy that whole section!
		for _, cz := range m.detail.copyZones {
			if msg.Y == cz.y && msg.X >= cz.x1 && msg.X < cz.x2 {
				return m, m.copyModalGroup(cz.group)
			}
		}
		// Press inside the modal body -> place cursor & begin potential drag selection!
		bodyStartY := r.y + 4
		bodyEndY := r.y + r.h - 3
		if msg.X > r.x && msg.X < r.x+r.w-1 && msg.Y >= bodyStartY && msg.Y < bodyEndY {
			m.beginModalDrag(msg.X, msg.Y)
			return m, nil
		}
		// Click action buttons on modal bottom action bar.
		if msg.Y == r.y+r.h-2 {
			for _, z := range m.detail.actionZones {
				if msg.X >= z.x1 && msg.X < z.x2 {
					m.detail.btnIdx = z.idx
					return m, m.triggerDetailAction(z.idx)
				}
			}
		}
		return m, nil
	}

	// Toolbar click (row 1).
	if msg.Y == 1 {
		m.clearSelection()
		for _, z := range m.tabZones {
			if msg.X >= z.x1 && msg.X < z.x2 {
				m.focus = focusToolbar
				m.activateToolbarControl(z.idx)
				return m, nil
			}
		}
		return m, nil
	}

	// Feed body press -> check inline card [ Copy ] button first; otherwise begin potential drag selection!
	if msg.Y >= m.bodyTopY && msg.Y < m.bodyTopY+m.bodyHeight {
		visIdx := m.scroll + (msg.Y - m.bodyTopY)
		if visIdx >= 0 && visIdx < len(m.rowTargets) {
			rt := m.rowTargets[visIdx]
			if rt.copyX2 > rt.copyX1 && msg.X >= rt.copyX1 && msg.X < rt.copyX2 {
				return m, m.copyEventNodeContent(rt.nodeID)
			}
		}
		m.beginFeedDrag(msg.X, msg.Y)
	}
	return m, nil
}

func (m *Model) ingestEvent(ev events.Event) {
	if ev.SessionID != "" {
		m.recordSessionEvent(ev)
	}
	hadNodes := len(m.nodeOrder) > 0
	m.feedDirty = true
	m.feedRev++

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
		if g, ok := m.groupByRun[ev.RunID]; ok {
			if g.endEv == nil && m.activeRuns > 0 {
				m.activeRuns--
			}
			evCopy := ev
			g.endEv = &evCopy
			g.opsCache.valid = false
			for _, ch := range g.children {
				if ch.ev.Tool == "code" {
					ch.ev.Ops = ev.Ops
					ch.ev.DurationMs = ev.DurationMs
				}
			}
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
						DurationMs: ev.DurationMs,
						HostOps:    ev.HostOps,
						Ops:        ev.Ops,
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
			if m.detail.open && m.detail.isExecGroup && m.detail.runID == ev.RunID {
				_, _, mw, _ := m.computeModalBounds()
				badge, absPath, metaLine, lines := buildDetailContent(g.startEv, g.endEv, max(48, mw-6))
				m.detail.badgeText = badge
				if absPath != "" {
					m.detail.absPath = absPath
				}
				m.detail.metaLine = metaLine
				m.detail.lines = lines
				m.detail.streaming = false
				if m.detail.query != "" {
					m.recomputeDetailMatches()
				}
				visCount := len(m.buildModalDisplayRows(m.modalContentWidth()))
				m.detail.totalVis = visCount
				if m.detail.cursor >= visCount {
					m.detail.cursor = max(0, visCount-1)
				}
			}
		}

	case events.KindTxStatus:
		if g, ok := m.groupByRun[ev.RunID]; ok {
			for _, ch := range g.children {
				if ch.ev.TxID == ev.TxID && ev.TxID != "" {
					ch.ev.TxPhase = ev.TxPhase
					ch.cache.valid = false
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
		if sr.child != nil {
			sr.child.cache.valid = false
		} else if sr.group != nil {
			sr.group.bodyCache.valid = false
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
		// In-place update when a running shell/task stream receives a replay update or transitions to its terminal state.
		if ev.StreamID != "" {
			if sr, exists := m.streamByID[ev.StreamID]; exists && sr != nil {
				target := sr.eventPtr()
				if ev.Cwd == "" {
					ev.Cwd = target.Cwd
				}
				if ev.Detail == "" && target.Detail != "" {
					ev.Detail = target.Detail
					ev.Truncated = ev.Truncated || target.Truncated
				}
				*target = ev
				if sr.child != nil {
					sr.child.cache.valid = false
				} else if sr.group != nil {
					sr.group.bodyCache.valid = false
				}
				if ev.Status != "running" {
					if sr.countedActive && m.activeRuns > 0 {
						sr.countedActive = false
						m.activeRuns--
					}
					delete(m.streamByID, ev.StreamID)
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
			if ev.StreamID != "" && ev.Status == "running" {
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
			if ev.StreamID != "" && ev.Status == "running" {
				sr := &streamRef{nodeID: g.id, group: g, countedActive: true}
				m.activeRuns++
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
		if drop.isExec && drop.endEv == nil && m.activeRuns > 0 {
			m.activeRuns--
		}
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

func (m *Model) recordSessionEvent(ev events.Event) {
	sid := strings.TrimSpace(ev.SessionID)
	if sid == "" {
		return
	}
	info, ok := m.sessionByID[sid]
	if !ok {
		info = &sessionInfo{
			id:       sid,
			colorIdx: len(m.sessions) % len(sessionAccentStyles),
		}
		m.sessionByID[sid] = info
		m.sessions = append(m.sessions, info)
	}
	if ev.SessionLabel != "" {
		info.label = ev.SessionLabel
	}
	if ev.Cwd != "" {
		info.cwd = ev.Cwd
	}
	if ts := shortTimestamp(ev.Timestamp); ts != "" {
		info.lastTs = ts
	}
	switch ev.Kind {
	case events.KindExecStart:
		info.runs++
		info.active++
	case events.KindExecEnd:
		if info.active > 0 {
			info.active--
		}
	case events.KindFileCreate, events.KindFileUpdate, events.KindFileDelete, events.KindFileRename:
		info.crud++
	case events.KindTask:
		if ev.Status == "running" {
			info.tasks++
			info.active++
		} else if info.active > 0 && ev.StreamID != "" {
			info.active--
		}
	}
}

func (m *Model) sessionDisplayName(sid string) string {
	info := m.sessionByID[sid]
	short := shortSessionID(sid)
	if info != nil && info.label != "" {
		return info.label
	}
	return "#" + short
}

func shortSessionID(sid string) string {
	if len(sid) <= 8 {
		return sid
	}
	return sid[:8]
}

func (m *Model) activeSessionFilter() string {
	return m.activeSessionID
}

func (m *Model) clearFeed() {
	m.clearSelection()
	m.activeRuns = 0
	m.groups = nil
	m.groupByRun = map[string]*runGroup{}
	m.streamByID = map[string]*streamRef{}
	m.sessions = nil
	m.sessionByID = map[string]*sessionInfo{}
	m.activeSessionID = ""
	m.nodeOrder = nil
	m.rowTargets = nil
	m.feedRows = nil
	m.lastFeedPlain = nil
	m.feedDirty = true
	m.feedRev++
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
			m.feedDirty = true
			m.feedRev++
			return
		}
		for _, ch := range g.children {
			if ch.id == id {
				ch.expanded = !ch.expanded
				m.feedDirty = true
				m.feedRev++
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
	m.feedDirty = true
	m.feedRev++
}

func (m *Model) syncFeedLayout() {
	rows, targets := m.ensureFeedRows()
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

func buildDetailContent(ev events.Event, endEv *events.Event, wrapW int) (badge, absPath, metaLine string, lines []detailLine) {
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
	durMs := ev.DurationMs
	if durMs == 0 && endEv != nil {
		durMs = endEv.DurationMs
	}
	if ev.Status == "running" && endEv == nil {
		metaParts = append(metaParts, "Status: running (live)")
	} else if durMs > 0 {
		metaParts = append(metaParts, fmt.Sprintf("%dms", durMs))
	}
	if ev.Truncated {
		metaParts = append(metaParts, "preview capped")
	}
	metaLine = strings.Join(metaParts, "  ·  ")

	if ev.Diff != nil && len(ev.Diff.Hunks) > 0 {
		diffLang := detectLang(ev.Path, "", "", "")
		if diffLang != "text" {
			metaParts = append(metaParts, "Lang: "+diffLang)
			metaLine = strings.Join(metaParts, "  ·  ")
		}
		lines = renderDiffDetailLines(ev.Diff, diffLang)
		return strings.TrimSpace(badge), absPath, metaLine, lines
	}

	ops := ev.Ops
	if len(ops) == 0 && endEv != nil {
		ops = endEv.Ops
	}
	if len(ops) > 0 {
		lines = append(lines, renderOpsChainDetailLines(ops, max(48, wrapW))...)
		lines = append(lines, detailLine{})
	}

	isExecModal := ev.Kind == events.KindExecStart && ev.Tool == "exec" && endEv != nil
	if isExecModal {
		// Structured GrokNight Code + Output recessed cards inside exec detail popup
		if strings.TrimSpace(ev.Detail) != "" {
			codeText := dedentBlock(ev.Detail)
			codeLines := strings.Split(codeText, "\n")
			hlCode := highlightLines(codeLines, "typescript")
			lines = append(lines, buildModalSectionHeader("CODE", "TypeScript", fmt.Sprintf("%d lines", len(codeLines)), colorCodeWell, true, 1))
			for i, l := range codeLines {
				gutter := styleCodeLineNum.Render(fmt.Sprintf("%5d │ ", i+1))
				lines = append(lines, detailLine{
					gutter:   gutter,
					plain:    l,
					body:     hlCode[i],
					bgHex:    colorCodeWell,
					panGroup: 1,
				})
			}
			lines = append(lines, detailLine{})
		}
		outText := endEv.Detail
		if outText == "" {
			outText = endEv.Error
		}
		if outText == "" {
			outText = "(no output returned)"
		}
		outLines := splitSanitizedLines(outText)
		hlOut := highlightLines(outLines, "output")
		outStatus := "OK"
		outOk := endEv.Status == "ok" || endEv.Status == ""
		if !outOk {
			outStatus = strings.ToUpper(endEv.Status)
		}
		outMeta := fmt.Sprintf("%d lines", len(outLines))
		if endEv.DurationMs > 0 {
			outMeta = fmt.Sprintf("%d lines · %dms", len(outLines), endEv.DurationMs)
		}
		lines = append(lines, buildModalSectionHeader("OUTPUT", outStatus, outMeta, colorCodeWell, outOk, 2))
		for i, l := range outLines {
			gutter := styleCodeLineNum.Render(fmt.Sprintf("%5d │ ", i+1))
			lines = append(lines, detailLine{
				gutter:   gutter,
				plain:    l,
				body:     hlOut[i],
				bgHex:    colorCodeWell,
				panGroup: 2,
			})
		}
		return strings.TrimSpace(badge), absPath, metaLine, lines
	}

	body := ev.Detail
	if endEv != nil && endEv.Detail != "" {
		if body != "" {
			body += "\n\n" + endEv.Detail
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
		if ev.Status == "running" && endEv == nil {
			body = "(running — waiting for output…)"
		} else {
			body = "(no detailed payload recorded)"
		}
	}

	lang := detectLang(ev.Path, ev.DetailKind, ev.Tool, body)
	if lang != "text" && lang != "output" {
		metaParts = append(metaParts, "Lang: "+lang)
		metaLine = strings.Join(metaParts, "  ·  ")
	}
	if lang == "typescript" && (ev.DetailKind == "code" || ev.Tool == "code") {
		body = dedentBlock(body)
	}
	rawLines := splitSanitizedLines(body)
	hlLines := highlightLines(rawLines, lang)
	useCodeWell := lang != "text" || ev.Kind == events.KindFileRead
	wellBg := ""
	if useCodeWell {
		wellBg = colorCodeWell
	}
	for i, l := range rawLines {
		gutter := styleLineNum.Render(fmt.Sprintf("%5d │ ", i+1))
		if useCodeWell {
			gutter = styleCodeLineNum.Render(fmt.Sprintf("%5d │ ", i+1))
		}
		lines = append(lines, detailLine{
			gutter:   gutter,
			plain:    l,
			body:     hlLines[i],
			bgHex:    wellBg,
			panGroup: 1,
		})
	}
	return strings.TrimSpace(badge), absPath, metaLine, lines
}

func buildModalSectionHeader(tag, label, meta, _ string, okTone bool, forGroup int) detailLine {
	tagStyle := styleSectionPillCode
	lblStyle := styleMuted
	if tag == "OUTPUT" {
		if okTone {
			tagStyle = styleSectionPillOk
			lblStyle = styleBadgeOk
		} else {
			tagStyle = styleSectionPillErr
			lblStyle = styleBadgeErr
		}
	}
	plain := fmt.Sprintf("▸ %s · %s  (%s)", tag, label, meta)
	body := styleAccent.Render("▸ ") + tagStyle.Render(" "+tag+" ") + " " + lblStyle.Render(label) + "  " + styleDim.Render("·  "+meta)
	return detailLine{
		plain:          plain,
		body:           body,
		bgHex:          colorSurfaceHi,
		headerForGroup: forGroup,
	}
}

func (m *Model) openDetailForEvent(ev events.Event, endEv *events.Event) {
	m.clearSelection()
	title := formatOpCallTitle(ev)
	_, _, mw, _ := m.computeModalBounds()
	badge, absPath, metaLine, lines := buildDetailContent(ev, endEv, max(48, mw-6))
	isStreaming := ev.Status == "running" && ev.StreamID != ""
	isExecGrp := ev.Kind == events.KindExecStart && ev.Tool == "exec"

	m.detail = detailState{
		open:         true,
		focus:        modalFocusBody,
		runID:        ev.RunID,
		isExecGroup:  isExecGrp,
		streamID:     ev.StreamID,
		streaming:    isStreaming || (isExecGrp && endEv == nil),
		followTail:   isStreaming,
		wrap:         m.detailWrap,
		title:        title,
		badgeText:    badge,
		absPath:      absPath,
		oldAbsPath:   ev.OldAbsPath,
		cwd:          ev.Cwd,
		metaLine:     metaLine,
		lines:        lines,
		scroll:       0,
		cursor:       0,
		groupHScroll: nil,
		btnIdx:       0,
		hoverBtn:     -1,
		hoverRow:     -1,
	}
	if isStreaming {
		m.jumpDetailBottom()
	}
}

func (m *Model) refreshOpenDetailForStream(ev events.Event) {
	if !m.detail.open || m.detail.streamID == "" || m.detail.streamID != ev.StreamID {
		return
	}
	_, _, mw, _ := m.computeModalBounds()
	badge, absPath, metaLine, lines := buildDetailContent(ev, nil, max(48, mw-6))
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
	visCount := len(m.buildModalDisplayRows(m.modalContentWidth()))
	m.detail.totalVis = visCount
	page := m.modalPageHeight()
	maxScroll := max(0, visCount-page)
	if m.detail.followTail {
		m.detail.scroll = maxScroll
		m.detail.cursor = max(0, visCount-1)
	} else {
		if m.detail.scroll > maxScroll {
			m.detail.scroll = maxScroll
		}
		if m.detail.cursor >= visCount {
			m.detail.cursor = max(0, visCount-1)
		}
	}
}

func renderDiffDetailLines(d *diff.Result, lang string) []detailLine {
	var out []detailLine
	for hi, h := range d.Hunks {
		if hi > 0 {
			sep := fmt.Sprintf("  ⋯   │  @@ -%d,%d +%d,%d @@", h.OldStart, h.OldCount, h.NewStart, h.NewCount)
			out = append(out, detailLine{plain: sep, body: styleHunkSep.Render(sep)})
		}
		for _, dl := range h.Lines {
			var gutter, sign, body, bgHex string
			switch dl.Kind {
			case diff.LineAdd:
				sign = "+ "
				gutter = styleDiffAddNum.Render(fmt.Sprintf("%4d │ ", dl.NewLine))
				body = renderSpannedLine(sign, dl.Text, dl.Spans, styleDiffAddBg, styleDiffAddHi)
				bgHex = colorDiffAddBg
			case diff.LineDel:
				sign = "- "
				gutter = styleDiffDelNum.Render(fmt.Sprintf("%4d │ ", dl.OldLine))
				body = renderSpannedLine(sign, dl.Text, dl.Spans, styleDiffDelBg, styleDiffDelHi)
				bgHex = colorDiffDelBg
			default:
				sign = "  "
				n := dl.NewLine
				if n == 0 {
					n = dl.OldLine
				}
				gutter = styleLineNum.Render(fmt.Sprintf("%4d │ ", n))
				body = sign + highlightLineByLang(dl.Text, lang)
			}
			out = append(out, detailLine{
				gutter:   gutter,
				plain:    sign + sanitizePlainLine(dl.Text),
				body:     body,
				bgHex:    bgHex,
				panGroup: 1,
			})
		}
	}
	if d.Truncated {
		note := fmt.Sprintf("  ⋯   │  ... diff truncated (total +%d -%d lines)", d.Added, d.Removed)
		out = append(out, detailLine{plain: note, body: styleHunkSep.Render(note)})
	}
	return out
}

func (m *Model) modalContentWidth() int {
	_, _, mw, _ := m.computeModalBounds()
	innerW := max(20, mw-2)
	return max(10, innerW-2) // 1 left margin/cursor col + 1 right scrollbar col
}

func (m *Model) moveDetailCursor(delta int) {
	visRows := m.buildModalDisplayRows(m.modalContentWidth())
	total := len(visRows)
	m.detail.totalVis = total
	if total == 0 {
		m.detail.cursor = 0
		m.detail.scroll = 0
		return
	}
	page := m.modalPageHeight()
	m.detail.cursor = max(0, min(total-1, m.detail.cursor+delta))
	if m.detail.cursor < m.detail.scroll {
		m.detail.scroll = m.detail.cursor
	} else if m.detail.cursor >= m.detail.scroll+page {
		m.detail.scroll = m.detail.cursor - page + 1
	}
	maxScroll := max(0, total-page)
	m.detail.scroll = max(0, min(maxScroll, m.detail.scroll))
	if m.sel.active && m.sel.scope == selScopeModal && m.sel.lineMode {
		m.sel.focusRow = m.detail.cursor
	}
	if delta < 0 {
		m.detail.followTail = false
	} else if m.detail.cursor >= total-1 {
		m.detail.followTail = true
	}
}

func (m *Model) scrollDetail(delta int) {
	visRows := m.buildModalDisplayRows(m.modalContentWidth())
	total := len(visRows)
	m.detail.totalVis = total
	page := m.modalPageHeight()
	maxScroll := max(0, total-page)
	m.detail.scroll = max(0, min(maxScroll, m.detail.scroll+delta))
	if total > 0 {
		if m.detail.cursor < m.detail.scroll {
			m.detail.cursor = m.detail.scroll
		} else if m.detail.cursor >= m.detail.scroll+page {
			m.detail.cursor = min(total-1, m.detail.scroll+page-1)
		}
		if m.sel.active && m.sel.scope == selScopeModal && m.sel.lineMode {
			m.sel.focusRow = m.detail.cursor
		}
	}
	if delta < 0 {
		m.detail.followTail = false
	} else if m.detail.scroll >= maxScroll {
		m.detail.followTail = true
	}
}

func (m *Model) jumpDetailTop() {
	m.detail.scroll = 0
	m.detail.cursor = 0
	m.detail.followTail = false
	if m.sel.active && m.sel.scope == selScopeModal && m.sel.lineMode {
		m.sel.focusRow = 0
	}
}

func (m *Model) jumpDetailBottom() {
	visRows := m.buildModalDisplayRows(m.modalContentWidth())
	total := len(visRows)
	m.detail.totalVis = total
	page := m.modalPageHeight()
	m.detail.scroll = max(0, total-page)
	m.detail.cursor = max(0, total-1)
	m.detail.followTail = true
	if m.sel.active && m.sel.scope == selScopeModal && m.sel.lineMode {
		m.sel.focusRow = m.detail.cursor
	}
}

func detailPanGroupAtRow(visRows []modalVisRow, idx int) int {
	if idx < 0 || idx >= len(visRows) {
		return 0
	}
	vr := visRows[idx]
	if vr.panGroup > 0 {
		return vr.panGroup
	}
	if vr.headerForGroup > 0 {
		return vr.headerForGroup
	}
	return 0
}

func (m *Model) detailActivePanGroup(visRows []modalVisRow) int {
	return detailPanGroupAtRow(visRows, m.detail.cursor)
}

func (m *Model) detailGroupHScroll(group int) int {
	if m.detail.wrap || group <= 0 || m.detail.groupHScroll == nil {
		return 0
	}
	return m.detail.groupHScroll[group]
}

func (m *Model) setDetailGroupHScroll(group, offset int) {
	if group <= 0 {
		return
	}
	if offset <= 0 {
		if m.detail.groupHScroll != nil {
			delete(m.detail.groupHScroll, group)
		}
		return
	}
	if m.detail.groupHScroll == nil {
		m.detail.groupHScroll = make(map[int]int)
	}
	m.detail.groupHScroll[group] = offset
}

func detailGroupMaxHScroll(visRows []modalVisRow, contentW, group int) int {
	if group <= 0 {
		return 0
	}
	maxH := 0
	for _, vr := range visRows {
		if vr.panGroup != group {
			continue
		}
		gw := ansi.StringWidth(vr.gutter)
		avail := max(4, contentW-gw)
		if over := vr.bodyWidth - avail; over > maxH {
			maxH = over
		}
	}
	return maxH
}

func (m *Model) activeDetailHScroll() int {
	if m.detail.wrap {
		return 0
	}
	contentW := m.modalContentWidth()
	visRows := m.buildModalDisplayRows(contentW)
	grp := m.detailActivePanGroup(visRows)
	if grp <= 0 {
		return 0
	}
	maxH := detailGroupMaxHScroll(visRows, contentW, grp)
	return min(m.detailGroupHScroll(grp), maxH)
}

func (m *Model) panDetailGroup(visRows []modalVisRow, contentW, group, delta int) {
	if m.detail.wrap || group <= 0 {
		return
	}
	maxH := detailGroupMaxHScroll(visRows, contentW, group)
	cur := min(m.detailGroupHScroll(group), maxH)
	m.setDetailGroupHScroll(group, max(0, min(maxH, cur+delta)))
}

func (m *Model) panDetailHorizontal(delta int) {
	if m.detail.wrap {
		return
	}
	contentW := m.modalContentWidth()
	visRows := m.buildModalDisplayRows(contentW)
	grp := m.detailActivePanGroup(visRows)
	m.panDetailGroup(visRows, contentW, grp, delta)
}

func (m *Model) panDetailHoverOrActive(delta int) {
	if m.detail.wrap {
		return
	}
	contentW := m.modalContentWidth()
	visRows := m.buildModalDisplayRows(contentW)
	grp := detailPanGroupAtRow(visRows, m.detail.hoverRow)
	if grp <= 0 {
		grp = m.detailActivePanGroup(visRows)
	}
	m.panDetailGroup(visRows, contentW, grp, delta)
}

func (m *Model) resetActiveDetailHScroll() {
	if m.detail.wrap {
		return
	}
	contentW := m.modalContentWidth()
	visRows := m.buildModalDisplayRows(contentW)
	grp := m.detailActivePanGroup(visRows)
	m.setDetailGroupHScroll(grp, 0)
}

func (m *Model) panDetailToGroupEnd() {
	if m.detail.wrap {
		return
	}
	contentW := m.modalContentWidth()
	visRows := m.buildModalDisplayRows(contentW)
	grp := m.detailActivePanGroup(visRows)
	if grp <= 0 {
		return
	}
	maxH := detailGroupMaxHScroll(visRows, contentW, grp)
	m.setDetailGroupHScroll(grp, maxH)
}

func (m *Model) toggleDetailWrap() {
	contentW := m.modalContentWidth()
	oldRows := m.buildModalDisplayRows(contentW)
	curLogical := 0
	if m.detail.cursor >= 0 && m.detail.cursor < len(oldRows) {
		curLogical = oldRows[m.detail.cursor].logicalIdx
	}
	m.detail.wrap = !m.detail.wrap
	m.detailWrap = m.detail.wrap
	m.detail.groupHScroll = nil

	newRows := m.buildModalDisplayRows(contentW)
	m.detail.totalVis = len(newRows)
	newCursor := 0
	for i, vr := range newRows {
		if vr.logicalIdx >= curLogical {
			newCursor = i
			break
		}
	}
	m.detail.cursor = newCursor
	page := m.modalPageHeight()
	maxScroll := max(0, len(newRows)-page)
	m.detail.scroll = max(0, min(maxScroll, newCursor-page/3))
}

func compileSearchRegex(q string) *regexp.Regexp {
	if strings.TrimSpace(q) == "" {
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
	logicalIdx := m.detail.matches[cursor]
	contentW := m.modalContentWidth()
	visRows := m.buildModalDisplayRows(contentW)
	m.detail.totalVis = len(visRows)
	visIdx := logicalIdx
	for i, vr := range visRows {
		if vr.logicalIdx == logicalIdx {
			visIdx = i
			break
		}
	}
	page := m.modalPageHeight()
	m.detail.cursor = max(0, min(max(0, len(visRows)-1), visIdx))
	targetScroll := max(0, m.detail.cursor-page/3)
	maxScroll := max(0, len(visRows)-page)
	m.detail.scroll = min(maxScroll, targetScroll)
	m.detail.followTail = false

	// When horizontal pan is active (wrap=false), auto-pan the match's section block so off-screen match jumps into view!
	if !m.detail.wrap && logicalIdx >= 0 && logicalIdx < len(m.detail.lines) {
		dl := m.detail.lines[logicalIdx]
		if dl.panGroup > 0 {
			if re := compileSearchRegex(m.detail.query); re != nil {
				if loc := re.FindStringIndex(dl.plain); loc != nil {
					matchCol := ansi.StringWidth(dl.plain[:loc[0]])
					gw := ansi.StringWidth(dl.gutter)
					avail := max(8, contentW-gw)
					curH := m.detailGroupHScroll(dl.panGroup)
					if matchCol < curH || matchCol >= curH+avail-4 {
						maxH := detailGroupMaxHScroll(visRows, contentW, dl.panGroup)
						m.setDetailGroupHScroll(dl.panGroup, max(0, min(maxH, matchCol-8)))
					}
				}
			}
		}
	}
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
	s = sanitizeStyledLine(s)
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
