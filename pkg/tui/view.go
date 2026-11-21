package tui

import (
	"fmt"
	"regexp"
	"strings"

	"github.com/charmbracelet/x/ansi"

	"github.com/aliothmoon/mcplex/pkg/diff"
	"github.com/aliothmoon/mcplex/pkg/events"
)

const maxInlinePreviewLines = 24

func (m *Model) View() string {
	m.recomputeLayoutMetrics()
	base := m.renderMainView()
	if !m.detail.open {
		return base
	}
	return m.compositeFloatingModal(base)
}

func (m *Model) renderMainView() string {
	header := m.renderHeaderBar()
	tabBar := m.renderTabBar()

	divStyle := styleDivider
	if m.focus == focusToolbar && !m.detail.open {
		divStyle = styleDividerFocused
	}
	divider := paintLineBg(divStyle.Render(strings.Repeat("─", m.width)), colorCanvas, m.width)

	rows, targets := m.buildFeedRows()
	m.rowTargets = targets
	m.totalVisRows = len(rows)

	maxScroll := max(0, len(rows)-m.bodyHeight)
	if m.follow {
		m.scroll = maxScroll
	} else if m.scroll > maxScroll {
		m.scroll = maxScroll
	}

	var body []string
	if len(rows) == 0 {
		body = m.renderEmptyState()
	} else {
		// Compute subtle 1-col right-edge feed scrollbar thumb range when content overflows.
		scrollable := len(rows) > m.bodyHeight && m.bodyHeight > 0
		thumbStart, thumbEnd := 0, 0
		if scrollable {
			thumbSize := max(1, (m.bodyHeight*m.bodyHeight)/len(rows))
			thumbStart = (m.scroll * (m.bodyHeight - thumbSize)) / max(1, maxScroll)
			thumbEnd = min(m.bodyHeight, thumbStart+thumbSize)
		}
		feedW := max(10, m.width-1)
		for i := 0; i < m.bodyHeight; i++ {
			idx := m.scroll + i
			var line string
			if idx < len(rows) {
				line = rows[idx]
			} else {
				line = paintLineBg("", colorCanvas, feedW)
			}
			sb := paintLineBg(" ", colorCanvas, 1)
			if scrollable {
				if i >= thumbStart && i < thumbEnd {
					sb = paintLineBg(styleScrollThumb.Render("┃"), colorCanvas, 1)
				} else {
					sb = paintLineBg(styleScrollTrack.Render("│"), colorCanvas, 1)
				}
			}
			body = append(body, line+sb)
		}
	}

	footer := m.renderFooterBar()

	var all []string
	all = append(all, header, tabBar, divider)
	all = append(all, body...)
	all = append(all, footer)
	return strings.Join(all, "\n")
}

func (m *Model) renderEmptyState() []string {
	lines := make([]string, m.bodyHeight)
	for i := range lines {
		lines[i] = paintLineBg("", colorCanvas, m.width)
	}

	cardW := min(m.width-6, 74)
	if cardW < 40 {
		// Compact fallback on narrow terminals
		msg := "  " + styleBrandMark.Render("◆") + " " + styleGroupText.Render("Listening on "+m.cfg.Endpoint)
		if m.bodyHeight > 1 {
			lines[1] = paintLineBg(msg, colorCanvas, m.width)
		}
		return lines
	}
	innerW := cardW - 2
	leftPad := max(1, (m.width-cardW)/2)
	bdr := styleCardBorder

	var card []string
	card = append(card, bdr.Render("╭"+strings.Repeat("─", innerW)+"╮"))

	if m.activeTab != tabAll {
		row1 := "  " + styleOpSearch.Render("○") + "  " + styleGroupText.Render("No events in current filter")
		row2 := "     " + styleDim.Render("Use ") + styleFooterKey.Render("← →") + styleDim.Render(" or press ") + styleFooterKey.Render("1") + styleDim.Render(" to return to All")
		card = append(card,
			bdr.Render("│")+paintLineBg("", colorSurface, innerW)+bdr.Render("│"),
			bdr.Render("│")+paintLineBg(row1, colorSurface, innerW)+bdr.Render("│"),
			bdr.Render("│")+paintLineBg(row2, colorSurface, innerW)+bdr.Render("│"),
			bdr.Render("│")+paintLineBg("", colorSurface, innerW)+bdr.Render("│"),
		)
	} else {
		titleRow := "  " + styleBrandMark.Render("◆") + "  " + styleBrand.Render("MCPlex Console") + "  " + styleDim.Render("v"+m.cfg.Version)
		statusRow := "     " + styleLiveDot.Render("●") + " " + styleMuted.Render("Listening on ") + stylePath.Render(m.cfg.Endpoint)
		subRow := "     " + styleDim.Render("Live diffs, file CRUD, search, shell & task streams appear here.")
		sepRow := bdr.Render("├" + strings.Repeat("─", innerW) + "┤")

		k1 := "  " + styleFooterKey.Render("↑ ↓") + styleDim.Render("  navigate feed        ") + styleFooterKey.Render("enter / click") + styleDim.Render("  fold & expand")
		k2 := "  " + styleFooterKey.Render("← →") + styleDim.Render("  toolbar & tree       ") + styleFooterKey.Render("o / dbl-click") + styleDim.Render("  inspect modal")
		k3 := "  " + styleFooterKey.Render("/") + styleDim.Render("    search & regex       ") + styleFooterKey.Render("f / G        ") + styleDim.Render("  live tail follow")

		card = append(card,
			bdr.Render("│")+paintLineBg("", colorSurface, innerW)+bdr.Render("│"),
			bdr.Render("│")+paintLineBg(titleRow, colorSurface, innerW)+bdr.Render("│"),
			bdr.Render("│")+paintLineBg(statusRow, colorSurface, innerW)+bdr.Render("│"),
			bdr.Render("│")+paintLineBg(subRow, colorSurface, innerW)+bdr.Render("│"),
			bdr.Render("│")+paintLineBg("", colorSurface, innerW)+bdr.Render("│"),
			sepRow,
			bdr.Render("│")+paintLineBg("", colorSurface, innerW)+bdr.Render("│"),
			bdr.Render("│")+paintLineBg(k1, colorSurface, innerW)+bdr.Render("│"),
			bdr.Render("│")+paintLineBg(k2, colorSurface, innerW)+bdr.Render("│"),
			bdr.Render("│")+paintLineBg(k3, colorSurface, innerW)+bdr.Render("│"),
			bdr.Render("│")+paintLineBg("", colorSurface, innerW)+bdr.Render("│"),
		)
	}
	card = append(card, bdr.Render("╰"+strings.Repeat("─", innerW)+"╯"))

	startY := max(1, (m.bodyHeight-len(card))/2)
	for i, cLine := range card {
		y := startY + i
		if y >= 0 && y < m.bodyHeight {
			row := strings.Repeat(" ", leftPad) + cLine
			lines[y] = paintLineBg(row, colorCanvas, m.width)
		}
	}
	return lines
}

func (m *Model) renderHeaderBar() string {
	status := styleLiveDot.Render("● Live")
	if !m.connected {
		note := m.connNote
		if note == "" {
			note = "offline"
		}
		status = styleErrDot.Render("○ " + note)
	} else if m.hasActiveWork() {
		n := max(1, m.activeRuns)
		status = styleRunDot.Render(fmt.Sprintf("%s Running %d", m.currentSpinner(), n))
	}

	left := " " + styleBrandMark.Render("◆") + " " + styleBrand.Render("MCPlex") +
		styleDim.Render(" v"+m.cfg.Version) +
		styleFooterSep.Render("  ·  ") +
		styleHeaderBar.Render(m.cfg.Endpoint) +
		styleFooterSep.Render("  ·  ") + status

	var metrics []string
	metrics = append(metrics, fmt.Sprintf("%s %s", styleMuted.Render(fmt.Sprintf("%d", m.totalRuns)), styleDim.Render("runs")))
	metrics = append(metrics, fmt.Sprintf("%s %s", styleMuted.Render(fmt.Sprintf("%d", m.totalCRUD)), styleDim.Render("edits")))
	if m.totalTasks > 0 {
		metrics = append(metrics, fmt.Sprintf("%s %s", styleMuted.Render(fmt.Sprintf("%d", m.totalTasks)), styleDim.Render("tasks")))
	}
	if m.totalAdd > 0 || m.totalDel > 0 {
		metrics = append(metrics, styleOpCreate.Render(fmt.Sprintf("+%d", m.totalAdd))+" "+styleOpDelete.Render(fmt.Sprintf("-%d", m.totalDel)))
	}
	right := strings.Join(metrics, styleFooterSep.Render("  ·  ")) + " "

	return composeSplitRow(left, right, colorSurface, m.width)
}

func (m *Model) renderTabBar() string {
	m.tabZones = nil
	ctrls := m.toolbarControls()
	if m.toolbarIdx >= len(ctrls) {
		m.toolbarIdx = max(0, len(ctrls)-1)
	}

	type renderedCtrl struct {
		idx     int
		id      string
		isLeft  bool
		content string
		width   int
	}

	var leftCtrls, rightCtrls []renderedCtrl
	for i, c := range ctrls {
		focused := m.focus == focusToolbar && !m.detail.open && i == m.toolbarIdx
		hovered := !m.detail.open && i == m.hoverToolbar
		activeTab := c.isTab && m.activeTab == c.tab

		text := c.label
		if focused {
			text = "› " + text
		} else if activeTab {
			text = "● " + text
		}
		padded := " " + text + " "

		var rendered string
		switch {
		case focused:
			rendered = styleCtrlFocused.Render(padded)
		case hovered:
			rendered = styleCtrlHover.Render(padded)
		case activeTab:
			rendered = styleTabActiveUnfocused.Render(padded)
		case c.id == "follow" && m.follow:
			rendered = styleFollowOn.Render(padded)
		default:
			rendered = styleCtrlNormal.Render(padded)
		}

		item := renderedCtrl{
			idx:     i,
			id:      c.id,
			isLeft:  c.isTab || c.id == "session",
			content: rendered,
			width:   ansi.StringWidth(rendered),
		}
		if item.isLeft {
			leftCtrls = append(leftCtrls, item)
		} else {
			rightCtrls = append(rightCtrls, item)
		}
	}

	var leftW, rightW int
	for _, it := range leftCtrls {
		leftW += it.width + 1
	}
	for _, it := range rightCtrls {
		rightW += it.width + 1
	}
	gap := max(2, m.width-2-leftW-rightW)

	var b strings.Builder
	col := 1
	b.WriteString(" ")

	for _, it := range leftCtrls {
		m.tabZones = append(m.tabZones, hitZone{x1: col, x2: col + it.width, action: it.id, idx: it.idx})
		b.WriteString(it.content)
		b.WriteString(" ")
		col += it.width + 1
	}

	if col+rightW+1 <= m.width {
		b.WriteString(strings.Repeat(" ", gap))
		col += gap
	} else {
		sep := styleFooterSep.Render("│ ")
		b.WriteString(sep)
		col += ansi.StringWidth(sep)
	}

	for _, it := range rightCtrls {
		m.tabZones = append(m.tabZones, hitZone{x1: col, x2: col + it.width, action: it.id, idx: it.idx})
		b.WriteString(it.content)
		b.WriteString(" ")
		col += it.width + 1
	}

	return paintLineBg(b.String(), colorCanvas, m.width)
}

func (m *Model) renderFooterBar() string {
	if m.searchMode {
		prompt := " " + styleAccent.Render("/") + stylePath.Render(m.searchInput) + styleAccent.Render("█") +
			"   " + styleFooterKey.Render("enter") + styleFooterDesc.Render(" jump  ") +
			styleFooterKey.Render("esc") + styleFooterDesc.Render(" cancel")
		return paintLineBg(prompt, colorSurface, m.width)
	}

	sep := styleFooterSep.Render("  ·  ")
	hint := func(k, d string) string {
		return styleFooterKey.Render(k) + " " + styleFooterDesc.Render(d)
	}

	var left string
	if m.feedQuery != "" {
		left = " " + styleAccent.Render("find ") + stylePath.Render(fmt.Sprintf("%q", m.feedQuery)) +
			styleDim.Render(fmt.Sprintf(" (%d/%d)", min(len(m.feedMatches), m.feedMatchAt+1), len(m.feedMatches))) +
			sep + hint("n/N", "next/prev") + sep + hint("o", "inspect") + sep + hint("esc", "clear")
	} else if m.focus == focusToolbar {
		left = " " + hint("←→", "select") + sep + hint("enter", "apply") + sep + hint("↓/tab", "feed") + sep + hint("1-4", "filter") + sep + hint("q", "quit")
	} else {
		left = " " + hint("↑↓", "move") + sep + hint("enter", "fold") + sep + hint("o", "inspect") + sep + hint("/", "find") + sep + hint("f", "follow") + sep + hint("tab", "bar") + sep + hint("q", "quit")
	}

	modeTag := styleFooterTag.Render("FEED")
	if m.focus == focusToolbar {
		modeTag = styleFooterTag.Render("BAR")
	}
	posTxt := ""
	if m.totalVisRows > 0 {
		endRow := min(m.totalVisRows, m.scroll+m.bodyHeight)
		pct := min(100, (endRow*100)/max(1, m.totalVisRows))
		posTxt = styleFooterSep.Render(" · ") + styleDim.Render(fmt.Sprintf("%d%%", pct))
	}
	right := modeTag + posTxt + " "
	return composeSplitRow(left, right, colorSurface, m.width)
}

func (m *Model) buildFeedRows() ([]string, []rowTarget) {
	var rows []string
	var targets []rowTarget
	m.nodeOrder = m.nodeOrder[:0]
	feedW := max(10, m.width-1) // reserve 1 rightmost column for sleek scrollbar

	appendRow := func(text string, rt rowTarget) {
		rows = append(rows, text)
		targets = append(targets, rt)
	}

	shownGroups := 0
	for _, g := range m.groups {
		if !m.groupMatchesTab(g) {
			continue
		}
		if shownGroups > 0 {
			// Grok Build CLI breathing room between turns
			appendRow(paintLineBg("", colorCanvas, feedW), rowTarget{})
		}
		shownGroups++

		m.nodeOrder = append(m.nodeOrder, g.id)
		sel := m.focus == focusFeed && g.id == m.selectedID
		hov := g.id == m.hoverNodeID && !sel
		appendRow(m.formatGroupHeader(g, sel, hov, feedW), rowTarget{nodeID: g.id, action: "toggle"})

		if !g.expanded {
			continue
		}

		if !g.isExec {
			bodyLines, moreCount := renderEventBodyLines(g.startEv, "     ", feedW-6)
			for _, bl := range bodyLines {
				appendRow(paintLineBg(bl, colorCanvas, feedW), rowTarget{nodeID: g.id, action: "select"})
			}
			if moreCount > 0 || g.startEv.Truncated || g.startEv.Detail != "" || g.startEv.Status == "running" || (g.startEv.Diff != nil && len(g.startEv.Diff.Hunks) > 0) {
				link := "     " + renderInspectLink(moreCount, g.startEv, sel || hov)
				appendRow(paintLineBg(link, colorCanvas, feedW), rowTarget{nodeID: g.id, action: "inspect"})
			}
			continue
		}

		var visChildren []*childItem
		for _, ch := range g.children {
			if m.eventMatchesTab(ch.ev) {
				visChildren = append(visChildren, ch)
			}
		}

		for ci, ch := range visChildren {
			isLast := ci == len(visChildren)-1
			m.nodeOrder = append(m.nodeOrder, ch.id)
			cSel := m.focus == focusFeed && ch.id == m.selectedID
			cHov := ch.id == m.hoverNodeID && !cSel
			appendRow(m.formatChildHeader(ch, isLast, cSel, cHov, feedW), rowTarget{nodeID: ch.id, action: "toggle"})

			if ch.expanded {
				branchPrefix := "    " + styleTreeConn.Render("│") + "   "
				if isLast {
					branchPrefix = "        "
				}
				bodyLines, moreCount := renderEventBodyLines(ch.ev, branchPrefix, feedW-9)
				for _, bl := range bodyLines {
					appendRow(paintLineBg(bl, colorCanvas, feedW), rowTarget{nodeID: ch.id, action: "select"})
				}
				if moreCount > 0 || ch.ev.Truncated || ch.ev.Detail != "" || ch.ev.Status == "running" || (ch.ev.Diff != nil && len(ch.ev.Diff.Hunks) > 0) {
					link := branchPrefix + renderInspectLink(moreCount, ch.ev, cSel || cHov)
					appendRow(paintLineBg(link, colorCanvas, feedW), rowTarget{nodeID: ch.id, action: "inspect"})
				}
			}
		}
	}
	return rows, targets
}

func renderInspectLink(moreCount int, ev events.Event, highlight bool) string {
	var text string
	switch {
	case ev.Status == "running":
		if moreCount > 0 {
			text = fmt.Sprintf("╰─ ⋯ %d earlier lines  ·  o live tail", moreCount)
		} else {
			text = "╰─ live output  ·  o tail -f"
		}
		return styleInspectLive.Render(text)
	case moreCount > 0 && ev.DetailKind == "shell":
		text = fmt.Sprintf("╰─ ⋯ %d earlier lines  ·  o inspect", moreCount)
	case moreCount > 0:
		text = fmt.Sprintf("╰─ ⋯ %d more lines  ·  o inspect", moreCount)
	default:
		text = "╰─ o inspect full view"
	}
	if highlight {
		return styleInspectSel.Render(text)
	}
	return styleInspectQuiet.Render(text)
}

func formatOpCallTitle(ev events.Event) string {
	switch ev.Kind {
	case events.KindFileCreate:
		return fmt.Sprintf("Create(%s)", ev.Path)
	case events.KindFileUpdate:
		return fmt.Sprintf("Update(%s)", ev.Path)
	case events.KindFileDelete:
		return fmt.Sprintf("Delete(%s)", ev.Path)
	case events.KindFileRename:
		return fmt.Sprintf("Rename(%s → %s)", ev.OldPath, ev.Path)
	case events.KindFileRead:
		return fmt.Sprintf("Read(%s)", ev.Path)
	case events.KindSearch:
		return fmt.Sprintf("Search(%q)", ev.Title)
	case events.KindShell:
		return fmt.Sprintf("Shell(%s)", ev.Title)
	case events.KindTask:
		return fmt.Sprintf("%s(%s)", strings.ToUpper(ev.Tool), ev.Title)
	case events.KindTxStatus:
		return fmt.Sprintf("Tx(%s · %s)", ev.TxID, ev.TxPhase)
	default:
		if ev.Tool == "code" {
			return "Code(TypeScript)"
		}
		if ev.Tool == "result" {
			return "Result"
		}
		if ev.Title != "" {
			return fmt.Sprintf("%s · %s", ev.Tool, ev.Title)
		}
		return ev.Tool
	}
}

func (m *Model) rowBgHex(selected, hovered bool) string {
	if selected {
		return colorSurfaceSel
	}
	if hovered {
		return colorHoverRow
	}
	return colorCanvas
}

func (m *Model) formatGroupHeader(g *runGroup, selected, hovered bool, feedW int) string {
	arrow := styleArrow.Render("▸")
	if g.expanded {
		arrow = styleArrow.Render("▾")
	}
	if selected {
		if g.expanded {
			arrow = styleArrowSel.Render("▾")
		} else {
			arrow = styleArrowSel.Render("▸")
		}
	}
	rail := " "
	if selected {
		rail = styleSelectedBar.Render("▎")
	}
	ts := shortTimestamp(g.startEv.Timestamp)
	bgHex := m.rowBgHex(selected, hovered)

	if !g.isExec {
		dot, title := m.renderStyledOpCall(g.startEv)
		left := fmt.Sprintf("%s %s %s %s", rail, arrow, dot, title)
		var rightParts []string
		if g.startEv.Summary != "" {
			if g.startEv.Status == "running" {
				rightParts = append(rightParts, styleRunDot.Render(g.startEv.Summary))
			} else {
				rightParts = append(rightParts, styleMuted.Render(g.startEv.Summary))
			}
		}
		if g.startEv.DurationMs > 0 && !strings.Contains(g.startEv.Summary, "ms") {
			rightParts = append(rightParts, styleDim.Render(fmt.Sprintf("%dms", g.startEv.DurationMs)))
		}
		if ts != "" {
			rightParts = append(rightParts, styleDim.Render(ts))
		}
		right := strings.Join(rightParts, styleFooterSep.Render("  ·  ")) + " "
		return composeSplitRow(left, right, bgHex, feedW)
	}

	dot := styleRunDot.Render(m.currentSpinner())
	var rightParts []string
	if g.endEv != nil {
		if g.endEv.Status == "ok" {
			dot = styleBadgeOk.Render("●")
		} else {
			dot = styleBadgeErr.Render("✕")
		}
		if g.addedSum > 0 || g.removedSum > 0 {
			rightParts = append(rightParts, styleOpCreate.Render(fmt.Sprintf("+%d", g.addedSum))+" "+styleOpDelete.Render(fmt.Sprintf("-%d", g.removedSum)))
		}
		rightParts = append(rightParts, styleMuted.Render(fmt.Sprintf("%d ops", g.endEv.HostOps)))
		rightParts = append(rightParts, styleDim.Render(fmt.Sprintf("%dms", g.endEv.DurationMs)))
	} else {
		rightParts = append(rightParts, styleRunDot.Render("running"))
	}
	if ts != "" {
		rightParts = append(rightParts, styleDim.Render(ts))
	}

	title := g.startEv.Title
	if title == "" {
		title = "exec"
	}
	left := fmt.Sprintf("%s %s %s %s %s",
		rail, arrow, dot,
		styleDim.Render("exec"),
		styleGroupText.Render(title))
	right := strings.Join(rightParts, styleFooterSep.Render("  ·  ")) + " "
	return composeSplitRow(left, right, bgHex, feedW)
}

func (m *Model) formatChildHeader(ch *childItem, isLast, selected, hovered bool, feedW int) string {
	arrow := styleArrow.Render("▸")
	if ch.expanded {
		arrow = styleArrow.Render("▾")
	}
	if selected {
		if ch.expanded {
			arrow = styleArrowSel.Render("▾")
		} else {
			arrow = styleArrowSel.Render("▸")
		}
	}
	conn := "├─"
	if isLast {
		conn = "└─"
	}
	rail := "  "
	if selected {
		rail = styleSelectedBar.Render("▎ ")
	}

	ev := ch.ev
	dot, callText := m.renderStyledOpCall(ev)

	var rightParts []string
	if ev.Diff != nil && (ev.Diff.Added > 0 || ev.Diff.Removed > 0) {
		rightParts = append(rightParts, styleOpCreate.Render(fmt.Sprintf("+%d", ev.Diff.Added))+" "+styleOpDelete.Render(fmt.Sprintf("-%d", ev.Diff.Removed)))
	} else if ev.Summary != "" {
		if ev.Status == "running" {
			rightParts = append(rightParts, styleRunDot.Render(ev.Summary))
		} else {
			rightParts = append(rightParts, styleDim.Render(ev.Summary))
		}
	}
	if ev.TxID != "" {
		switch ev.TxPhase {
		case "committed":
			rightParts = append(rightParts, styleBadgeOk.Render(ev.TxID+" ✓"))
		case "rolled_back":
			rightParts = append(rightParts, styleBadgeWrn.Render(ev.TxID+" ↺"))
		default:
			rightParts = append(rightParts, styleDim.Render(ev.TxID))
		}
	}

	left := fmt.Sprintf("%s  %s %s %s %s",
		rail, styleTreeConn.Render(conn), arrow, dot, callText)
	right := ""
	if len(rightParts) > 0 {
		right = strings.Join(rightParts, styleFooterSep.Render("  ·  ")) + " "
	}
	return composeSplitRow(left, right, m.rowBgHex(selected, hovered), feedW)
}

// composeSplitRow aligns left and right segments cleanly onto a solid background without clipping right metrics.
func composeSplitRow(left, right, bgHex string, width int) string {
	if width <= 0 {
		return ""
	}
	rightW := ansi.StringWidth(right)
	if rightW == 0 {
		return paintLineBg(left, bgHex, width)
	}
	if rightW >= width-4 {
		return paintLineBg(left+"  "+right, bgHex, width)
	}
	maxLeft := width - rightW - 2
	leftClamped := clampLineWidth(left, maxLeft)
	gap := max(2, width-ansi.StringWidth(leftClamped)-rightW)
	return paintLineBg(leftClamped+strings.Repeat(" ", gap)+right, bgHex, width)
}

func (m *Model) renderStyledOpCall(ev events.Event) (dot string, call string) {
	switch ev.Kind {
	case events.KindFileCreate:
		return styleOpCreate.Render("●"), styleOpCreate.Render("Create") + styleDim.Render("(") + stylePath.Render(ev.Path) + styleDim.Render(")")
	case events.KindFileUpdate:
		return styleOpUpdate.Render("●"), styleOpUpdate.Render("Update") + styleDim.Render("(") + stylePath.Render(ev.Path) + styleDim.Render(")")
	case events.KindFileDelete:
		return styleOpDelete.Render("●"), styleOpDelete.Render("Delete") + styleDim.Render("(") + stylePath.Render(ev.Path) + styleDim.Render(")")
	case events.KindFileRename:
		return styleOpRename.Render("●"), styleOpRename.Render("Rename") + styleDim.Render("(") + stylePath.Render(ev.OldPath+" → "+ev.Path) + styleDim.Render(")")
	case events.KindFileRead:
		return styleOpRead.Render("●"), styleOpRead.Render("Read") + styleDim.Render("(") + stylePath.Render(ev.Path) + styleDim.Render(")")
	case events.KindSearch:
		return styleOpSearch.Render("●"), styleOpSearch.Render("Search") + styleDim.Render("(") + stylePath.Render(ev.Title) + styleDim.Render(")")
	case events.KindShell:
		switch ev.Status {
		case "running":
			dot = styleRunDot.Render(m.currentSpinner())
		case "error", "timeout", "canceled":
			dot = styleBadgeErr.Render("✕")
		default:
			dot = styleOpShell.Render("●")
		}
		return dot, styleOpShell.Render("Shell") + styleDim.Render("(") + stylePath.Render(ev.Title) + styleDim.Render(")")
	case events.KindTask:
		switch ev.Status {
		case "running":
			dot = styleRunDot.Render(m.currentSpinner())
		case "failed", "error", "killed", "timed_out":
			dot = styleBadgeErr.Render("✕")
		case "completed", "ok":
			dot = styleBadgeOk.Render("●")
		default:
			dot = styleOpTask.Render("●")
		}
		return dot, styleOpTask.Render(strings.ToUpper(ev.Tool)) + styleDim.Render("(") + stylePath.Render(ev.Title) + styleDim.Render(")")
	case events.KindTxStatus:
		if ev.TxPhase == "committed" {
			return styleBadgeOk.Render("✓"), styleBadgeOk.Render("Tx(" + ev.TxID + " committed)")
		}
		return styleBadgeWrn.Render("↺"), styleBadgeWrn.Render("Tx(" + ev.TxID + " " + ev.TxPhase + ")")
	default:
		if ev.Tool == "code" {
			return styleOpCode.Render("◦"), styleOpCode.Render("Code(TypeScript)")
		}
		if ev.Tool == "result" {
			if ev.Status != "" && ev.Status != "ok" {
				return styleBadgeErr.Render("✕"), styleBadgeErr.Render("Error / Output")
			}
			return styleOpCode.Render("◦"), styleOpCode.Render("Result")
		}
		return styleOpCode.Render("●"), styleOpCode.Render(formatOpCallTitle(ev))
	}
}

func renderEventBodyLines(ev events.Event, pad string, maxW int) ([]string, int) {
	var out []string
	totalLines := 0
	stripW := max(24, maxW-2)

	if ev.Diff != nil && len(ev.Diff.Hunks) > 0 {
		for hi, h := range ev.Diff.Hunks {
			if hi > 0 {
				if len(out) < maxInlinePreviewLines {
					out = append(out, pad+styleHunkSep.Render(fmt.Sprintf("  ⋯  │  @@ -%d,%d +%d,%d @@", h.OldStart, h.OldCount, h.NewStart, h.NewCount)))
				}
				totalLines++
			}
			for _, dl := range h.Lines {
				totalLines++
				if len(out) < maxInlinePreviewLines {
					out = append(out, pad+renderColoredDiffLine(dl, stripW))
				}
			}
		}
		more := max(0, totalLines-len(out))
		return out, more
	}

	if ev.Error != "" {
		out = append(out, pad+styleCodeRail.Render("│ ")+styleBadgeErr.Render("Error: "+ev.Error))
	}
	if ev.Detail == "" && ev.Status == "running" {
		out = append(out, pad+styleCodeRail.Render("│ ")+styleDim.Render("waiting for output…"))
		return out, 0
	}
	if ev.Detail != "" {
		raw := strings.Split(strings.ReplaceAll(strings.TrimRight(ev.Detail, "\n"), "\r\n", "\n"), "\n")
		totalLines = len(raw)
		if ev.DetailKind == "shell" && len(raw) > maxInlinePreviewLines {
			// Show the latest tail lines so streaming logs visibly tick live in the feed.
			start := len(raw) - maxInlinePreviewLines
			for i := start; i < len(raw); i++ {
				out = append(out, fmt.Sprintf("%s%s%s", pad, styleLineNum.Render(fmt.Sprintf("%4d │ ", i+1)), styleDiffCtx.Render(raw[i])))
			}
			return out, start
		}
		for i, l := range raw {
			if len(out) >= maxInlinePreviewLines {
				break
			}
			out = append(out, fmt.Sprintf("%s%s%s", pad, styleLineNum.Render(fmt.Sprintf("%4d │ ", i+1)), styleDiffCtx.Render(l)))
		}
		more := max(0, totalLines-len(out))
		return out, more
	}
	return out, 0
}

func formatDiffGutter(dl diff.DiffLine) string {
	var numStr string
	sign := "  "
	switch dl.Kind {
	case diff.LineAdd:
		numStr = fmt.Sprintf("%4d │ ", dl.NewLine)
		sign = styleOpCreate.Render("+ ")
	case diff.LineDel:
		numStr = fmt.Sprintf("%4d │ ", dl.OldLine)
		sign = styleOpDelete.Render("- ")
	default:
		n := dl.NewLine
		if n == 0 {
			n = dl.OldLine
		}
		numStr = fmt.Sprintf("%4d │ ", n)
	}
	return styleLineNum.Render(numStr) + sign
}

func renderColoredDiffLine(dl diff.DiffLine, stripW int) string {
	var numStr string
	switch dl.Kind {
	case diff.LineAdd:
		numStr = fmt.Sprintf("%4d │ ", dl.NewLine)
		inner := styleDiffAddNum.Render(numStr) + renderSpannedLine("+ ", dl.Text, dl.Spans, styleDiffAddBg, styleDiffAddHi)
		if stripW > 0 {
			return paintDiffStrip(inner, colorDiffAddBg, stripW)
		}
		return inner
	case diff.LineDel:
		numStr = fmt.Sprintf("%4d │ ", dl.OldLine)
		inner := styleDiffDelNum.Render(numStr) + renderSpannedLine("- ", dl.Text, dl.Spans, styleDiffDelBg, styleDiffDelHi)
		if stripW > 0 {
			return paintDiffStrip(inner, colorDiffDelBg, stripW)
		}
		return inner
	default:
		n := dl.NewLine
		if n == 0 {
			n = dl.OldLine
		}
		numStr = fmt.Sprintf("%4d │ ", n)
		return styleLineNum.Render(numStr) + styleDiffCtx.Render("  "+dl.Text)
	}
}

func renderSpannedLine(sign, text string, spans []diff.Span, baseStyle, hiStyle interface{ Render(...string) string }) string {
	if len(spans) == 0 {
		return baseStyle.Render(sign + text)
	}
	var sb strings.Builder
	sb.WriteString(baseStyle.Render(sign))
	for _, sp := range spans {
		if sp.Start < 0 || sp.End > len(text) || sp.Start >= sp.End {
			continue
		}
		chunk := text[sp.Start:sp.End]
		if sp.Changed {
			sb.WriteString(hiStyle.Render(chunk))
		} else {
			sb.WriteString(baseStyle.Render(chunk))
		}
	}
	return sb.String()
}

// compositeFloatingModal renders a GrokNight elevated rounded popup with soft drop shadow over a dimmed obsidian scrim.
func (m *Model) compositeFloatingModal(baseView string) string {
	mx, my, mw, mh := m.computeModalBounds()
	m.detail.rect = modalRect{x: mx, y: my, w: mw, h: mh}

	popupLines := m.buildModalBox(mx, my, mw, mh)
	baseLines := strings.Split(baseView, "\n")
	for len(baseLines) < m.height {
		baseLines = append(baseLines, "")
	}

	out := make([]string, len(baseLines))
	for y := 0; y < len(baseLines); y++ {
		plain := ansi.Strip(baseLines[y])
		// Bottom drop-shadow row (1 row directly under modal, shifted +2 cols)
		if y == my+mh && mx+2 < m.width {
			shStart := mx + 2
			shEnd := min(m.width, mx+mw+2)
			leftPlain := padExactWidth(ansi.Cut(plain, 0, shStart), shStart)
			rightPlain := padExactWidth(ansi.Cut(plain, shEnd, m.width), max(0, m.width-shEnd))
			out[y] = styleBackdrop.Render(leftPlain) +
				styleShadow.Render(strings.Repeat(" ", max(0, shEnd-shStart))) +
				styleBackdrop.Render(rightPlain)
			continue
		}
		if y < my || y >= my+mh {
			out[y] = styleBackdrop.Render(padExactWidth(plain, m.width))
			continue
		}
		leftPlain := padExactWidth(ansi.Cut(plain, 0, mx), mx)
		rightStart := mx + mw
		rightW := max(0, m.width-rightStart)

		// Soft 2-col right drop shadow for rows strictly below the top border
		if y > my && rightW >= 2 {
			afterShadow := rightStart + 2
			rightPlain := padExactWidth(ansi.Cut(plain, afterShadow, m.width), max(0, m.width-afterShadow))
			out[y] = styleBackdrop.Render(leftPlain) + popupLines[y-my] + styleShadow.Render("  ") + styleBackdrop.Render(rightPlain)
		} else {
			rightPlain := padExactWidth(ansi.Cut(plain, rightStart, m.width), rightW)
			out[y] = styleBackdrop.Render(leftPlain) + popupLines[y-my] + styleBackdrop.Render(rightPlain)
		}
	}
	return strings.Join(out, "\n")
}

func (m *Model) buildModalBox(mx, my, mw, mh int) []string {
	innerW := max(20, mw-2)
	bTop := styleModalBorderTop
	bdr := styleModalBorder

	// Row 0: Rounded top border with crisp title, optional live badge, and subtle [ esc × ] chip
	titlePrefix := " " + styleBrandMark.Render("◆") + " "
	if m.detail.streaming {
		titlePrefix = " " + styleRunDot.Render(m.currentSpinner()+" LIVE") + styleFooterSep.Render(" · ")
	}
	badgePart := ""
	if m.detail.badgeText != "" {
		badgePart = "  " + styleModalBadge.Render(" "+m.detail.badgeText+" ")
	}
	closeTxt := " esc × "
	closeRendered := styleModalClose.Render(closeTxt)
	if m.detail.hoverBtn == -2 {
		closeRendered = styleModalCloseHover.Render(closeTxt)
	}
	closeW := ansi.StringWidth(closeRendered)
	maxTitleW := max(4, innerW-closeW-3)
	titleCombined := paintLineBg(titlePrefix+styleModalTitle.Render(m.detail.title)+badgePart+" ", colorSurface, min(maxTitleW, ansi.StringWidth(titlePrefix)+ansi.StringWidth(m.detail.title)+ansi.StringWidth(badgePart)+1))
	titleW := ansi.StringWidth(titleCombined)
	fillDash := max(1, innerW-1-titleW-closeW-1)

	closeStartX := mx + 1 + 1 + titleW + fillDash
	m.detail.closeZone = hitZone{x1: closeStartX, x2: closeStartX + closeW, action: "close"}

	topRow := bTop.Render("╭─") + titleCombined + bTop.Render(strings.Repeat("─", fillDash)) + closeRendered + bTop.Render("─╮")

	// Row 1: Elevated metadata strip (#181B22) with full Absolute Path
	pathVal := m.detail.absPath
	if m.detail.oldAbsPath != "" {
		pathVal = m.detail.oldAbsPath + " → " + pathVal
	}
	if pathVal == "" {
		pathVal = "(no file path)"
	}
	pathRowContent := "  " + styleModalKey.Render("PATH") + "  " + styleModalPath.Render(pathVal)
	row1 := bdr.Render("│") + paintLineBg(pathRowContent, colorSurfaceHi, innerW) + bdr.Render("│")

	// Row 2: Elevated metadata strip (#181B22) with Tool / Cwd / Match summary
	metaTxt := m.detail.metaLine
	if m.detail.query != "" {
		if len(m.detail.matches) == 0 {
			metaTxt += fmt.Sprintf("  ·  Match %q: 0", m.detail.query)
		} else {
			metaTxt += fmt.Sprintf("  ·  Match %q: %d/%d", m.detail.query, m.detail.matchCursor+1, len(m.detail.matches))
		}
	}
	metaRowContent := "  " + styleModalKey.Render("INFO") + "  " + styleModalMeta.Render(metaTxt)
	row2 := bdr.Render("│") + paintLineBg(metaRowContent, colorSurfaceHi, innerW) + bdr.Render("│")

	// Row 3: Subtle hairline divider
	divRow := bdr.Render("├") + paintLineBg(styleDividerModal.Render(strings.Repeat("─", innerW)), colorSurface, innerW) + bdr.Render("┤")

	// Scrollable body rows on solid #13151A canvas + right-edge scrollbar
	page := max(3, mh-7)
	maxScroll := max(0, len(m.detail.lines)-page)
	if m.detail.scroll > maxScroll {
		m.detail.scroll = maxScroll
	}
	contentW := max(10, innerW-2) // 1 left margin + 1 right scrollbar column

	re := compileSearchRegex(m.detail.query)
	curMatchLine := -1
	if len(m.detail.matches) > 0 && m.detail.matchCursor >= 0 && m.detail.matchCursor < len(m.detail.matches) {
		curMatchLine = m.detail.matches[m.detail.matchCursor]
	}

	thumbStart, thumbEnd := 0, page
	if len(m.detail.lines) > page && page > 0 {
		thumbSize := max(1, (page*page)/len(m.detail.lines))
		thumbStart = (m.detail.scroll * (page - thumbSize)) / max(1, maxScroll)
		thumbEnd = min(page, thumbStart+thumbSize)
	}

	bodyRows := make([]string, page)
	for i := 0; i < page; i++ {
		lineIdx := m.detail.scroll + i
		var content string
		rowBg := colorSurface
		if lineIdx < len(m.detail.lines) {
			dl := m.detail.lines[lineIdx]
			if dl.bgHex != "" {
				rowBg = dl.bgHex
			}
			content = dl.styled
			if m.detail.query != "" && re != nil && re.MatchString(dl.plain) {
				hi := styleSearchMatch
				if lineIdx == curMatchLine {
					hi = styleSearchCur
				}
				content = hi.Render("›") + dl.gutter + highlightPlainQuery(dl.plain, re, hi)
			}
		}
		sbChar := styleModalScrollTrack.Render("│")
		if len(m.detail.lines) > page && i >= thumbStart && i < thumbEnd {
			sbChar = styleModalScrollThumb.Render("┃")
		} else if len(m.detail.lines) <= page {
			sbChar = paintLineBg(" ", colorSurface, 1)
		}
		bodyRows[i] = bdr.Render("│") + paintLineBg(" "+content, rowBg, contentW+1) + sbChar + bdr.Render("│")
	}

	// Bottom action / search bar inside modal
	m.detail.actionZones = nil
	var actionRow string
	if m.detail.searchMode {
		prompt := "  " + styleAccent.Render("/") + stylePath.Render(m.detail.searchInput) + styleAccent.Render("█") +
			styleDim.Render(fmt.Sprintf("   %d matches  ·  enter jump  ·  esc cancel", len(m.detail.matches)))
		actionRow = bdr.Render("│") + paintLineBg(prompt, colorSurfaceHi, innerW) + bdr.Render("│")
	} else {
		var ab strings.Builder
		col := mx + 1
		ab.WriteString(" ")
		col++

		btns := []string{"/ Search", "▲ Prev", "▼ Next", "Top", "End"}
		for idx, lbl := range btns {
			focused := idx == m.detail.btnIdx
			hovered := idx == m.detail.hoverBtn
			txt := " " + lbl + " "
			if focused {
				txt = " › " + lbl + " "
			}
			var rendered string
			switch {
			case focused:
				rendered = styleCtrlFocused.Render(txt)
			case hovered:
				rendered = styleCtrlHover.Render(txt)
			default:
				rendered = styleModalBtn.Render(txt)
			}
			w := ansi.StringWidth(rendered)
			m.detail.actionZones = append(m.detail.actionZones, hitZone{x1: col, x2: col + w, idx: idx})
			ab.WriteString(rendered)
			col += w
			ab.WriteString(" ")
			col++
		}

		endLine := min(len(m.detail.lines), m.detail.scroll+page)
		pct := 100
		if len(m.detail.lines) > 0 {
			pct = min(100, (endLine*100)/len(m.detail.lines))
		}
		liveTag := ""
		if m.detail.streaming {
			if m.detail.followTail {
				liveTag = styleRunDot.Render(m.currentSpinner()+" tail -f") + styleFooterSep.Render("  ·  ")
			} else {
				liveTag = styleDim.Render("paused (G)") + styleFooterSep.Render("  ·  ")
			}
		}
		posInfo := liveTag + styleModalMeta.Render(fmt.Sprintf("%d-%d / %d  %d%% ",
			min(len(m.detail.lines), m.detail.scroll+1), endLine, len(m.detail.lines), pct))
		usedW := (col - (mx + 1)) + ansi.StringWidth(posInfo)
		if gap := innerW - usedW; gap > 0 {
			ab.WriteString(strings.Repeat(" ", gap))
			ab.WriteString(posInfo)
		}
		actionRow = bdr.Render("│") + paintLineBg(ab.String(), colorSurfaceHi, innerW) + bdr.Render("│")
	}

	bottomRow := bdr.Render("╰" + strings.Repeat("─", innerW) + "╯")

	var box []string
	box = append(box, topRow, row1, row2, divRow)
	box = append(box, bodyRows...)
	box = append(box, divRow, actionRow, bottomRow)
	return box
}

func padExactWidth(plain string, width int) string {
	if width <= 0 {
		return ""
	}
	w := ansi.StringWidth(plain)
	if w > width {
		return ansi.Truncate(plain, width, "")
	}
	if w < width {
		return plain + strings.Repeat(" ", width-w)
	}
	return plain
}

func highlightPlainQuery(plain string, re *regexp.Regexp, hi interface{ Render(...string) string }) string {
	idxs := re.FindAllStringIndex(plain, -1)
	if len(idxs) == 0 {
		return plain
	}
	var sb strings.Builder
	last := 0
	for _, pair := range idxs {
		if pair[0] > last {
			sb.WriteString(plain[last:pair[0]])
		}
		sb.WriteString(hi.Render(plain[pair[0]:pair[1]]))
		last = pair[1]
	}
	if last < len(plain) {
		sb.WriteString(plain[last:])
	}
	return sb.String()
}

func shortTimestamp(ts string) string {
	if idx := strings.IndexByte(ts, 'T'); idx >= 0 && len(ts) >= idx+9 {
		return ts[idx+1 : idx+9]
	}
	return ts
}
