package tui

import (
	"regexp"
	"strings"
	"time"
	"unicode/utf8"

	tea "github.com/charmbracelet/bubbletea"
	"github.com/charmbracelet/x/ansi"

	"github.com/aliothmoon/mcplex/pkg/diff"
	"github.com/aliothmoon/mcplex/pkg/events"
)

type selScope int

const (
	selScopeNone selScope = iota
	selScopeFeed
	selScopeModal
)

type textSelection struct {
	scope          selScope
	active         bool
	dragging       bool
	moved          bool
	lineMode       bool // Vim-style 'v' visual line-select inside modal
	startX         int
	startY         int
	anchorRow      int // visual row index (modal visRows or feed rows)
	anchorCol      int // body display col (modal) or screen X col (feed)
	focusRow       int
	focusCol       int
	pendingFeedRow int
	hasPendingFeed bool
}

func (s textSelection) normalized() (r1, c1, r2, c2 int) {
	r1, c1 = s.anchorRow, s.anchorCol
	r2, c2 = s.focusRow, s.focusCol
	if r1 > r2 || (r1 == r2 && c1 > c2) {
		return r2, c2, r1, c1
	}
	return r1, c1, r2, c2
}

func (m *Model) clearSelection() {
	m.sel = textSelection{}
}

func (m *Model) performCopy(text string) tea.Cmd {
	if text == "" {
		return nil
	}
	m.copyToastSeq++
	if err := writeClipboard(text); err != nil {
		m.copyToast = "✕ Copy failed: " + err.Error()
		m.copyToastErr = true
	} else {
		m.copyToast = formatCopyToast(text)
		m.copyToastErr = false
	}
	return copyToastTimerCmd(m.copyToastSeq)
}

func (m *Model) toggleModalVisualSelect() {
	if m.sel.active && m.sel.scope == selScopeModal && m.sel.lineMode {
		m.clearSelection()
		return
	}
	m.copyToast = ""
	m.copyToastErr = false
	m.detail.focus = modalFocusBody
	m.sel = textSelection{
		scope:     selScopeModal,
		active:    true,
		lineMode:  true,
		anchorRow: m.detail.cursor,
		focusRow:  m.detail.cursor,
	}
}

func (m *Model) modalRowBodyLayout(visRows []modalVisRow, visIdx, contentW int) (bodyStartX, rowHScroll int) {
	mx, _, _, _ := m.computeModalBounds()
	if visIdx < 0 || visIdx >= len(visRows) {
		return mx + 2, 0
	}
	vr := visRows[visIdx]
	gw := ansi.StringWidth(vr.gutter)
	if !m.detail.wrap && vr.panGroup > 0 {
		maxH := detailGroupMaxHScroll(visRows, contentW, vr.panGroup)
		rowHScroll = min(m.detailGroupHScroll(vr.panGroup), maxH)
	}
	// mx: '│', mx+1: cursor rail '▎', mx+2: gutter start
	return mx + 2 + gw, rowHScroll
}

func (m *Model) screenToModalBodyPos(x, y int, allowScroll bool) (visIdx, bodyCol int, ok bool) {
	r := m.detail.rect
	bodyStartY := r.y + 4
	bodyEndY := r.y + r.h - 3
	if bodyEndY <= bodyStartY {
		return 0, 0, false
	}
	contentW := m.modalContentWidth()
	visRows := m.buildModalDisplayRows(contentW)
	total := len(visRows)
	m.detail.totalVis = total
	if total == 0 {
		return 0, 0, false
	}
	if allowScroll {
		if y < bodyStartY {
			m.scrollDetail(-1)
			y = bodyStartY
		} else if y >= bodyEndY {
			m.scrollDetail(1)
			y = bodyEndY - 1
		}
	}
	if y < bodyStartY {
		y = bodyStartY
	} else if y >= bodyEndY {
		y = bodyEndY - 1
	}
	visIdx = m.detail.scroll + (y - bodyStartY)
	visIdx = max(0, min(total-1, visIdx))

	bodyStartX, rowHScroll := m.modalRowBodyLayout(visRows, visIdx, contentW)
	relX := x - bodyStartX
	if relX < 0 {
		relX = 0
	}
	return visIdx, rowHScroll + relX, true
}

func (m *Model) beginModalDrag(x, y int) {
	visIdx, bodyCol, ok := m.screenToModalBodyPos(x, y, false)
	if !ok {
		return
	}
	m.detail.focus = modalFocusBody
	m.detail.cursor = visIdx
	m.detail.followTail = visIdx >= max(0, m.detail.totalVis-1)
	m.sel = textSelection{
		scope:     selScopeModal,
		active:    false,
		dragging:  true,
		moved:     false,
		startX:    x,
		startY:    y,
		anchorRow: visIdx,
		anchorCol: bodyCol,
		focusRow:  visIdx,
		focusCol:  bodyCol,
	}
}

func (m *Model) beginFeedDrag(x, y int) {
	visIdx := m.scroll + (y - m.bodyTopY)
	hasRow := visIdx >= 0 && visIdx < len(m.rowTargets)
	col := max(0, min(max(0, m.width-2), x))
	m.sel = textSelection{
		scope:          selScopeFeed,
		active:         false,
		dragging:       true,
		moved:          false,
		startX:         x,
		startY:         y,
		anchorRow:      max(0, visIdx),
		anchorCol:      col,
		focusRow:       max(0, visIdx),
		focusCol:       col,
		pendingFeedRow: visIdx,
		hasPendingFeed: hasRow,
	}
}

func (m *Model) updateMouseDrag(x, y int) {
	if !m.sel.dragging {
		return
	}
	if x != m.sel.startX || y != m.sel.startY {
		m.sel.moved = true
		m.sel.active = true
	}
	switch m.sel.scope {
	case selScopeModal:
		visIdx, bodyCol, ok := m.screenToModalBodyPos(x, y, true)
		if !ok {
			return
		}
		m.sel.focusRow = visIdx
		m.sel.focusCol = bodyCol
		m.detail.cursor = visIdx
	case selScopeFeed:
		if y < m.bodyTopY {
			m.follow = false
			m.scrollFeed(-1)
			y = m.bodyTopY
		} else if y >= m.bodyTopY+m.bodyHeight {
			m.follow = false
			m.scrollFeed(1)
			y = m.bodyTopY + m.bodyHeight - 1
		}
		visIdx := m.scroll + (y - m.bodyTopY)
		maxRow := max(0, len(m.lastFeedPlain)-1)
		m.sel.focusRow = max(0, min(maxRow, visIdx))
		m.sel.focusCol = max(0, min(max(0, m.width-2), x))
	}
}

func (m *Model) finishMouseDrag(x, y int) tea.Cmd {
	if !m.sel.dragging {
		return nil
	}
	if m.sel.moved {
		m.updateMouseDrag(x, y)
	}
	m.sel.dragging = false

	if m.sel.moved && m.sel.active {
		var copied string
		switch m.sel.scope {
		case selScopeModal:
			copied = m.extractModalSelectionText()
		case selScopeFeed:
			copied = m.extractFeedSelectionText()
		}
		if copied != "" {
			return m.performCopy(copied)
		}
		m.clearSelection()
		return nil
	}

	pendingFeed := m.sel.scope == selScopeFeed && m.sel.hasPendingFeed
	pendingRow := m.sel.pendingFeedRow
	m.clearSelection()

	if pendingFeed && pendingRow >= 0 && pendingRow < len(m.rowTargets) {
		m.activateFeedRowClick(pendingRow)
	}
	return nil
}

func (m *Model) activateFeedRowClick(visIdx int) {
	rt := m.rowTargets[visIdx]
	if rt.nodeID == "" {
		return
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

func snapWideSelectionBounds(s string, totalW, sc, ec int) (int, int) {
	sc = max(0, min(totalW, sc))
	if sc > 0 && ansi.StringWidth(ansi.Cut(s, 0, sc)) < sc {
		sc--
	}
	ec = max(sc, min(totalW, ec))
	if ec < totalW && ansi.StringWidth(ansi.Cut(s, 0, ec)) < ec {
		ec++
	}
	return sc, ec
}

func (m *Model) extractModalSelectionText() string {
	if !m.sel.active || m.sel.scope != selScopeModal {
		return ""
	}
	contentW := m.modalContentWidth()
	visRows := m.buildModalDisplayRows(contentW)
	if len(visRows) == 0 {
		return ""
	}
	r1, c1, r2, c2 := m.sel.normalized()
	r1 = max(0, min(len(visRows)-1, r1))
	r2 = max(0, min(len(visRows)-1, r2))

	if m.sel.lineMode {
		var lines []string
		lastLog := -1
		for r := r1; r <= r2; r++ {
			li := visRows[r].logicalIdx
			if li == lastLog || li < 0 || li >= len(m.detail.lines) {
				continue
			}
			lastLog = li
			lines = append(lines, m.detail.lines[li].plain)
		}
		return strings.Join(lines, "\n")
	}

	var out strings.Builder
	for r := r1; r <= r2; r++ {
		vr := visRows[r]
		var plainRow string
		if !m.detail.wrap && vr.logicalIdx >= 0 && vr.logicalIdx < len(m.detail.lines) {
			plainRow = m.detail.lines[vr.logicalIdx].plain
		} else {
			plainRow = ansi.Strip(vr.bodyStyled)
		}
		rowW := ansi.StringWidth(plainRow)
		sc := 0
		ec := rowW
		if r == r1 {
			sc = max(0, min(rowW, c1))
		}
		if r == r2 {
			ec = max(sc, min(rowW, c2+1))
		}
		sc, ec = snapWideSelectionBounds(plainRow, rowW, sc, ec)
		seg := ansi.Cut(plainRow, sc, ec)
		if r > r1 {
			if !(vr.isCont && vr.logicalIdx == visRows[r-1].logicalIdx) {
				out.WriteByte('\n')
			}
		}
		out.WriteString(seg)
	}
	return out.String()
}

var reFeedCardLinePrefix = regexp.MustCompile(`^\s*[│├└─\s]*│\s*\d+\s*│\s?(.*)$`)

func (m *Model) extractFeedSelectionText() string {
	m.ensureFeedRows()
	if !m.sel.active || m.sel.scope != selScopeFeed || len(m.lastFeedPlain) == 0 {
		return ""
	}
	r1, c1, r2, c2 := m.sel.normalized()
	r1 = max(0, min(len(m.lastFeedPlain)-1, r1))
	r2 = max(0, min(len(m.lastFeedPlain)-1, r2))

	if r1 == r2 {
		plain := m.lastFeedPlain[r1]
		rowW := ansi.StringWidth(plain)
		sc, ec := snapWideSelectionBounds(plain, rowW, c1, c2+1)
		return strings.TrimRight(ansi.Cut(plain, sc, ec), " ")
	}

	var lines []string
	for r := r1; r <= r2; r++ {
		plain := m.lastFeedPlain[r]
		rowW := ansi.StringWidth(plain)
		sc := 0
		ec := rowW
		if r == r1 {
			sc = max(0, min(rowW, c1))
		}
		if r == r2 {
			ec = max(sc, min(rowW, c2+1))
		}
		sc, ec = snapWideSelectionBounds(plain, rowW, sc, ec)
		seg := strings.TrimRight(ansi.Cut(plain, sc, ec), " ")
		if mMatch := reFeedCardLinePrefix.FindStringSubmatch(seg); mMatch != nil {
			seg = strings.TrimRight(mMatch[1], " ")
			seg = strings.TrimRight(strings.TrimSuffix(seg, "│"), " ")
		}
		lines = append(lines, seg)
	}
	return strings.TrimRight(strings.Join(lines, "\n"), "\n")
}

func (m *Model) copyFromModal(copyAll bool) tea.Cmd {
	if !copyAll && m.sel.active && m.sel.scope == selScopeModal {
		text := m.extractModalSelectionText()
		if m.sel.lineMode {
			m.clearSelection()
		}
		return m.performCopy(text)
	}

	if copyAll {
		var all []string
		for _, dl := range m.detail.lines {
			all = append(all, dl.plain)
		}
		return m.performCopy(strings.Join(all, "\n"))
	}

	contentW := m.modalContentWidth()
	visRows := m.buildModalDisplayRows(contentW)
	grp := m.detailActivePanGroup(visRows)
	if grp > 0 {
		if cmd := m.copyModalGroup(grp); cmd != nil {
			return cmd
		}
	}
	if m.detail.cursor >= 0 && m.detail.cursor < len(visRows) {
		li := visRows[m.detail.cursor].logicalIdx
		if li >= 0 && li < len(m.detail.lines) {
			return m.performCopy(m.detail.lines[li].plain)
		}
	}
	return nil
}

func (m *Model) copyModalGroup(group int) tea.Cmd {
	if group <= 0 {
		return nil
	}
	var sec []string
	for _, dl := range m.detail.lines {
		if dl.panGroup == group {
			sec = append(sec, dl.plain)
		}
	}
	if len(sec) == 0 {
		return nil
	}
	return m.performCopy(strings.Join(sec, "\n"))
}

func (m *Model) copyFromFeed() tea.Cmd {
	if m.sel.active && m.sel.scope == selScopeFeed {
		return m.performCopy(m.extractFeedSelectionText())
	}
	return m.copyEventNodeContent(m.selectedID)
}

func (m *Model) copyEventNodeContent(nodeID string) tea.Cmd {
	if nodeID == "" {
		return nil
	}
	ev, endEv := m.findEventByNodeID(nodeID)
	if ev == nil {
		return nil
	}
	if ev.Diff != nil && len(ev.Diff.Hunks) > 0 {
		var lines []string
		for _, h := range ev.Diff.Hunks {
			for _, dl := range h.Lines {
				prefix := "  "
				if dl.Kind == diff.LineAdd {
					prefix = "+ "
				} else if dl.Kind == diff.LineDel {
					prefix = "- "
				}
				lines = append(lines, prefix+sanitizePlainLine(dl.Text))
			}
		}
		return m.performCopy(strings.Join(lines, "\n"))
	}
	if endEv != nil && endEv.Detail != "" {
		return m.performCopy(endEv.Detail)
	}
	if ev.Detail != "" {
		if ev.DetailKind == "code" || ev.Tool == "code" {
			return m.performCopy(dedentBlock(ev.Detail))
		}
		return m.performCopy(strings.TrimRight(ev.Detail, "\n"))
	}
	if ev.Error != "" {
		return m.performCopy(ev.Error)
	}
	if ev.AbsPath != "" {
		return m.performCopy(ev.AbsPath)
	}
	return m.performCopy(ev.Title)
}

func (m *Model) findEventByNodeID(nodeID string) (*events.Event, *events.Event) {
	for _, g := range m.groups {
		if g.id == nodeID {
			return &g.startEv, g.endEv
		}
		for _, ch := range g.children {
			if ch.id == nodeID {
				return &ch.ev, nil
			}
		}
	}
	return nil, nil
}

func (m *Model) feedRowOuterBgHex(visIdx int) string {
	if visIdx < 0 || visIdx >= len(m.rowTargets) {
		return colorCanvas
	}
	rt := m.rowTargets[visIdx]
	if rt.action != "toggle" || rt.nodeID == "" {
		return colorCanvas
	}
	sel := m.focus == focusFeed && rt.nodeID == m.selectedID
	hov := rt.nodeID == m.hoverNodeID && !sel
	return m.rowBgHex(sel, hov)
}

func (m *Model) highlightFeedRowSelection(line string, visIdx, feedW int) string {
	r1, c1, r2, c2 := m.sel.normalized()
	if visIdx < r1 || visIdx > r2 {
		return line
	}
	sc := 0
	ec := feedW
	if visIdx == r1 {
		sc = max(0, min(feedW, c1))
	}
	if visIdx == r2 {
		ec = max(sc, min(feedW, c2+1))
	}
	return applySelectionHighlightWithBg(line, feedW, sc, ec, m.feedRowOuterBgHex(visIdx))
}

// applySelectionHighlight tints display columns [selStart, selEnd) in styled with crisp cobalt
// selection colors while strictly preserving total display width and surrounding ANSI styling.
func applySelectionHighlight(styled string, totalW, selStart, selEnd int) string {
	return applySelectionHighlightWithBg(styled, totalW, selStart, selEnd, "")
}

func applySelectionHighlightWithBg(styled string, totalW, selStart, selEnd int, fallbackBgHex string) string {
	if totalW <= 0 {
		return styled
	}
	selStart, selEnd = snapWideSelectionBounds(styled, totalW, selStart, selEnd)
	if selStart >= selEnd {
		return styled
	}
	before := ansi.Cut(styled, 0, selStart)
	midPlain := ansi.Strip(ansi.Cut(styled, selStart, selEnd))
	midRendered := styleTextSel.Render(midPlain)
	if selEnd >= totalW {
		return before + midRendered
	}

	after := ansi.Cut(styled, selEnd, totalW)
	fallbackBg := ""
	if fallbackBgHex != "" {
		fallbackBg = bgEscapeSeq(fallbackBgHex)
	}
	activeBg := activeBgSGRAtCol(styled, selEnd)
	if activeBg == "" {
		activeBg = fallbackBg
	}
	if activeBg == "" {
		return before + midRendered + after
	}

	hasEndReset := strings.HasSuffix(after, "\x1b[0m")
	afterCore := strings.TrimSuffix(after, "\x1b[0m")
	afterCore = strings.TrimPrefix(strings.TrimPrefix(afterCore, "\x1b[0m"), "\x1b[m")
	if fallbackBg != "" {
		afterCore = reapplyBgAfterResets(afterCore, fallbackBg)
	}
	prefixBg := activeBg
	if strings.HasPrefix(afterCore, "\x1b[48;") {
		prefixBg = ""
	}
	tailReset := "\x1b[0m"
	if !hasEndReset && fallbackBg == "" {
		tailReset = ""
	}
	return before + midRendered + prefixBg + afterCore + tailReset
}

func activeBgSGRAtCol(styled string, targetCol int) string {
	curBg := ""
	col := 0
	for i := 0; i < len(styled); {
		if styled[i] == '\x1b' {
			if end := matchSGRSeq(styled, i); end > 0 {
				curBg = updateBgFromSGRParams(curBg, styled[i+2:end-1])
				i = end
				continue
			}
			i = skipEscapeSeq(styled, i)
			continue
		}
		if col >= targetCol {
			break
		}
		r, size := utf8.DecodeRuneInString(styled[i:])
		if size <= 0 {
			break
		}
		if r < 0x7f {
			col++
		} else if w := ansi.StringWidth(styled[i : i+size]); w > 0 {
			col += w
		}
		i += size
	}
	return curBg
}

func updateBgFromSGRParams(curBg, params string) string {
	if params == "" || params == "0" {
		return ""
	}
	parts := strings.Split(params, ";")
	for i := 0; i < len(parts); {
		p := parts[i]
		switch {
		case p == "" || p == "0" || p == "49":
			curBg = ""
			i++
		case strings.HasPrefix(p, "48:"):
			curBg = "\x1b[" + p + "m"
			i++
		case p == "48":
			if i+2 < len(parts) && parts[i+1] == "5" {
				curBg = "\x1b[48;5;" + parts[i+2] + "m"
				i += 3
			} else if i+4 < len(parts) && parts[i+1] == "2" {
				curBg = "\x1b[48;2;" + parts[i+2] + ";" + parts[i+3] + ";" + parts[i+4] + "m"
				i += 5
			} else {
				i++
			}
		case (len(p) == 2 && p[0] == '4' && p[1] >= '0' && p[1] <= '7') ||
			(len(p) == 3 && p[0] == '1' && p[1] == '0' && p[2] >= '0' && p[2] <= '7'):
			curBg = "\x1b[" + p + "m"
			i++
		default:
			i++
		}
	}
	return curBg
}
