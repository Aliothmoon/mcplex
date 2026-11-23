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
	if m.sessPicker.open {
		return m.compositeSessionPickerModal(base)
	}
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

	rows, targets := m.ensureFeedRows()
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
				if m.sel.active && m.sel.scope == selScopeFeed {
					line = m.highlightFeedRowSelection(line, idx, feedW)
				}
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

	listenVerb := "Listening on "
	if m.cfg.ModeLabel == "logs" {
		listenVerb = "Watching "
	}
	cardW := min(m.width-6, 74)
	if cardW < 40 {
		// Compact fallback on narrow terminals
		msg := "  " + styleBrandMark.Render("◆") + " " + styleGroupText.Render(listenVerb+m.cfg.Endpoint)
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
		oauthBadge := ""
		if m.cfg.OAuthEnabled {
			oauthBadge = "  " + styleTokenPill.Render(" OAuth 2.1 ")
		}
		titleRow := "  " + styleBrandMark.Render("◆") + "  " + styleBrand.Render(m.brandName()+" Console") + "  " + styleDim.Render("v"+m.cfg.Version) + oauthBadge
		var infoRows []string
		if strings.TrimSpace(m.cfg.PublicEndpoint) != "" {
			pubRow := "     " + styleLiveDot.Render("●") + " " + styleMuted.Render("Public MCP  ") + stylePath.Render(m.cfg.PublicEndpoint) + "  " + styleDim.Render("[u] copy")
			locRow := "       " + styleDim.Render("Local HTTP  "+m.cfg.Endpoint)
			infoRows = append(infoRows, pubRow, locRow)
		} else if m.cfg.ModeLabel == "logs" {
			statusRow := "     " + styleLiveDot.Render("●") + " " + styleMuted.Render("Watching ") + stylePath.Render(m.cfg.Endpoint)
			infoRows = append(infoRows, statusRow)
		} else {
			statusRow := "     " + styleLiveDot.Render("●") + " " + styleMuted.Render("Listening on ") + stylePath.Render(m.cfg.Endpoint) + "  " + styleDim.Render("[u] copy")
			infoRows = append(infoRows, statusRow)
		}
		if m.token != "" {
			shownTok := maskTokenTail(m.token)
			if m.tokenRevealed {
				shownTok = m.token
			}
			hints := "[t] copy  [p] show/hide"
			if m.cfg.RotateToken != nil {
				hints += "  [T] new"
			}
			tokRow := "     " + styleAccent.Render("◆") + " " + styleMuted.Render("Owner Token ") + styleGroupText.Render(shownTok) + "  " + styleDim.Render(hints)
			infoRows = append(infoRows, tokRow)
		} else if m.cfg.RotateToken != nil {
			tokRow := "     " + styleDim.Render("○ Auth Token  none (local loopback)  [t] generate token")
			infoRows = append(infoRows, tokRow)
		} else {
			subRow := "     " + styleDim.Render("Live diffs, file CRUD, search, shell & task streams appear here.")
			infoRows = append(infoRows, subRow)
		}
		sepRow := bdr.Render("├" + strings.Repeat("─", innerW) + "┤")

		k1 := "  " + styleFooterKey.Render("↑ ↓") + styleDim.Render("  navigate feed        ") + styleFooterKey.Render("enter / click") + styleDim.Render("  fold & expand")
		k2 := "  " + styleFooterKey.Render("← →") + styleDim.Render("  toolbar & tree       ") + styleFooterKey.Render("o / dbl-click") + styleDim.Render("  inspect modal")
		k3 := "  " + styleFooterKey.Render("/") + styleDim.Render("    search & regex       ") + styleFooterKey.Render("t / u        ") + styleDim.Render("  copy token / URL")
		if m.cfg.ModeLabel == "logs" {
			k3 = "  " + styleFooterKey.Render("/") + styleDim.Render("    search & regex       ") + styleFooterKey.Render("s / 1-5      ") + styleDim.Render("  workspace / filter")
		}

		card = append(card, bdr.Render("│")+paintLineBg("", colorSurface, innerW)+bdr.Render("│"))
		card = append(card, bdr.Render("│")+paintLineBg(titleRow, colorSurface, innerW)+bdr.Render("│"))
		for _, ir := range infoRows {
			card = append(card, bdr.Render("│")+paintLineBg(ir, colorSurface, innerW)+bdr.Render("│"))
		}
		card = append(card,
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

	prefix := " " + styleBrandMark.Render("◆") + " " + styleBrand.Render(m.brandName()) +
		styleDim.Render(" v"+m.cfg.Version) +
		styleFooterSep.Render("  ·  ")
	ep := m.primaryEndpoint()
	epStyled := styleHeaderBar.Render(ep)
	urlX1 := ansi.StringWidth(prefix)
	m.headerURLZone = hitZone{x1: urlX1, x2: urlX1 + ansi.StringWidth(epStyled), action: "copy_url"}

	authNote := ""
	if m.cfg.OAuthEnabled {
		authNote = styleFooterSep.Render("  ·  ") + styleAccent.Render("OAuth")
	} else if m.token != "" || m.cfg.AuthEnabled {
		authNote = styleFooterSep.Render("  ·  ") + styleMuted.Render("Auth")
	}
	left := prefix + epStyled + styleFooterSep.Render("  ·  ") + status + authNote

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
		focused := m.focus == focusToolbar && !m.detail.open && !m.sessPicker.open && i == m.toolbarIdx
		hovered := !m.detail.open && !m.sessPicker.open && i == m.hoverToolbar
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
		case c.id == "copy_token" || c.id == "gen_token" || c.id == "copy_url":
			rendered = styleTokenPill.Render(padded)
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
		tokHint := ""
		if m.token != "" || m.cfg.RotateToken != nil {
			tokHint = sep + hint("t/T", "token")
		}
		fk := "1-4"
		if m.cfg.ShowErrorsTab {
			fk = "1-5"
		}
		left = " " + hint("←→", "select") + sep + hint("enter", "apply") + sep + hint("↓/tab", "feed") + tokHint + sep + hint(fk, "filter") + sep + hint("q", "quit")
	} else {
		tokHint := ""
		if m.token != "" || m.cfg.RotateToken != nil {
			tokHint = sep + hint("t", "token")
		}
		left = " " + hint("↑↓", "move") + sep + hint("enter", "fold") + sep + hint("o", "inspect") + sep + hint("drag/y", "copy") + tokHint + sep + hint("/", "find") + sep + hint("q", "quit")
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
	toastPart := ""
	if m.copyToast != "" {
		ts := styleCopyToast
		if m.copyToastErr {
			ts = styleCopyToastErr
		}
		toastPart = ts.Render(" "+m.copyToast+" ") + styleFooterSep.Render("  ·  ")
	}
	right := toastPart + modeTag + posTxt + " "
	return composeSplitRow(left, right, colorSurface, m.width)
}

func (m *Model) computeStructureHash() uint64 {
	const offset64 = 14695981039346656037
	const prime64 = 1099511628211
	h := uint64(offset64)
	mix := func(v uint64) {
		h ^= v
		h *= prime64
	}
	mix(uint64(len(m.groups)))
	for _, g := range m.groups {
		var flags uint64
		if g.expanded {
			flags |= 1
		}
		if g.endEv != nil {
			flags |= 2
		}
		mix(flags ^ uint64(g.hScroll)<<8 ^ uint64(len(g.children))<<16)
		for _, ch := range g.children {
			var cf uint64
			if ch.expanded {
				cf = 1
			}
			mix(cf ^ uint64(ch.hScroll)<<8)
		}
	}
	return h
}

func (m *Model) currentFeedCacheKey() feedCacheKey {
	spin := 0
	if m.hasActiveWork() {
		spin = m.spinnerFrame
	}
	return feedCacheKey{
		width:           m.width,
		activeTab:       m.activeTab,
		activeSessionID: m.activeSessionID,
		multiSession:    len(m.sessions) >= 2,
		feedFocused:     m.focus == focusFeed,
		selectedID:      m.selectedID,
		hoverNodeID:     m.hoverNodeID,
		hoverCopyNodeID: m.hoverCopyNodeID,
		spinnerFrame:    spin,
		feedRev:         m.feedRev,
		structureHash:   m.computeStructureHash(),
	}
}

func (m *Model) ensureFeedRows() ([]string, []rowTarget) {
	key := m.currentFeedCacheKey()
	if !m.feedDirty && m.feedRows != nil && key == m.lastCacheKey {
		return m.feedRows, m.rowTargets
	}
	rows, plains, targets := m.rebuildFeedRows()
	m.feedRows = rows
	m.lastFeedPlain = plains
	m.rowTargets = targets
	m.totalVisRows = len(rows)
	m.feedDirty = false
	m.lastCacheKey = key
	return m.feedRows, m.rowTargets
}

func (m *Model) buildFeedRows() ([]string, []rowTarget) {
	return m.ensureFeedRows()
}

func renderCachedCardLines(cache *cardRenderCache, ev events.Event, pad string, maxW, feedW int, isLast, hoverCopy bool, hScroll int) {
	if cache.valid && cache.feedW == feedW && cache.isLast == isLast && cache.hoverCopy == hoverCopy && cache.hScroll == hScroll {
		return
	}
	bodyLines, moreCount, copyX1, copyX2 := renderEventBodyLines(ev, pad, maxW, hoverCopy)
	painted := make([]string, len(bodyLines))
	plains := make([]string, len(bodyLines))
	for i, bl := range bodyLines {
		p := paintLineBg(bl, colorCanvas, feedW)
		painted[i] = p
		plains[i] = ansi.Strip(p)
	}
	*cache = cardRenderCache{
		valid:     true,
		feedW:     feedW,
		isLast:    isLast,
		hoverCopy: hoverCopy,
		hScroll:   hScroll,
		painted:   painted,
		plains:    plains,
		moreCount: moreCount,
		copyX1:    copyX1,
		copyX2:    copyX2,
	}
}

func (m *Model) rebuildFeedRows() ([]string, []string, []rowTarget) {
	var rows []string
	var plains []string
	var targets []rowTarget
	m.nodeOrder = m.nodeOrder[:0]
	feedW := max(10, m.width-1) // reserve 1 rightmost column for sleek scrollbar

	spacerPainted := paintLineBg("", colorCanvas, feedW)
	spacerPlain := strings.Repeat(" ", feedW)

	appendRow := func(text string, rt rowTarget) {
		rows = append(rows, text)
		plains = append(plains, ansi.Strip(text))
		targets = append(targets, rt)
	}
	appendCachedRow := func(painted, plain string, rt rowTarget) {
		rows = append(rows, painted)
		plains = append(plains, plain)
		targets = append(targets, rt)
	}

	shownGroups := 0
	for _, g := range m.groups {
		if !m.groupMatchesTab(g) {
			continue
		}
		if shownGroups > 0 {
			appendCachedRow(spacerPainted, spacerPlain, rowTarget{})
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
			hoverCopy := m.hoverCopyNodeID == g.id
			renderCachedCardLines(&g.bodyCache, g.startEv, "     ", feedW-6, feedW, true, hoverCopy, g.hScroll)
			for bi, pLine := range g.bodyCache.painted {
				rt := rowTarget{nodeID: g.id, action: "select"}
				if bi == 0 && g.bodyCache.copyX2 > g.bodyCache.copyX1 {
					rt.copyX1 = g.bodyCache.copyX1
					rt.copyX2 = g.bodyCache.copyX2
				}
				appendCachedRow(pLine, g.bodyCache.plains[bi], rt)
			}
			moreCount := g.bodyCache.moreCount
			if moreCount > 0 || g.startEv.Truncated || g.startEv.Detail != "" || g.startEv.Status == "running" || (g.startEv.Diff != nil && len(g.startEv.Diff.Hunks) > 0) {
				link := "     " + renderInspectFooter(moreCount, g.startEv, sel || hov, max(24, feedW-8))
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

		if g.endEv != nil && len(g.endEv.Ops) > 0 {
			noChild := len(visChildren) == 0
			if !g.opsCache.valid || g.opsCache.feedW != feedW || g.opsCache.noChild != noChild {
				conn := "    " + styleTreeConn.Render("│") + "  "
				if noChild {
					conn = "    " + styleTreeConn.Render("╰─") + " "
				}
				opsLine := paintLineBg(conn+renderOpsChainInline(g.endEv.Ops), colorCanvas, feedW)
				g.opsCache = groupOpsCache{
					valid:   true,
					feedW:   feedW,
					noChild: noChild,
					painted: opsLine,
					plain:   ansi.Strip(opsLine),
				}
			}
			appendCachedRow(g.opsCache.painted, g.opsCache.plain, rowTarget{nodeID: g.id, action: "inspect"})
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
				hoverCopy := m.hoverCopyNodeID == ch.id
				renderCachedCardLines(&ch.cache, ch.ev, branchPrefix, feedW-9, feedW, isLast, hoverCopy, ch.hScroll)
				for bi, pLine := range ch.cache.painted {
					rt := rowTarget{nodeID: ch.id, action: "select"}
					if bi == 0 && ch.cache.copyX2 > ch.cache.copyX1 {
						rt.copyX1 = ch.cache.copyX1
						rt.copyX2 = ch.cache.copyX2
					}
					appendCachedRow(pLine, ch.cache.plains[bi], rt)
				}
				moreCount := ch.cache.moreCount
				if moreCount > 0 || ch.ev.Truncated || ch.ev.Detail != "" || ch.ev.Status == "running" || (ch.ev.Diff != nil && len(ch.ev.Diff.Hunks) > 0) {
					link := branchPrefix + renderInspectFooter(moreCount, ch.ev, cSel || cHov, max(24, feedW-11))
					appendRow(paintLineBg(link, colorCanvas, feedW), rowTarget{nodeID: ch.id, action: "inspect"})
				}
			}
		}
	}
	return rows, plains, targets
}

func isBoxedCardEvent(ev events.Event) bool {
	if ev.Diff != nil && len(ev.Diff.Hunks) > 0 {
		return false
	}
	if ev.Detail == "" {
		return false
	}
	return ev.DetailKind == "code" || ev.DetailKind == "shell" || ev.DetailKind == "search" ||
		ev.Tool == "code" || ev.Tool == "result" || ev.Tool == "docs" || ev.Kind == events.KindFileRead
}

func renderInspectFooter(moreCount int, ev events.Event, highlight bool, boxW int) string {
	if !isBoxedCardEvent(ev) || boxW < 30 {
		return renderInspectLink(moreCount, ev, highlight)
	}
	// Close the recessed code/output box seamlessly with a sleek bottom border + clickable inspect hint
	innerW := boxW - 2
	var label string
	var lblStyle interface{ Render(...string) string }
	switch {
	case ev.Status == "running":
		if moreCount > 0 {
			label = fmt.Sprintf(" ⋯ %d earlier lines · o live tail ", moreCount)
		} else {
			label = " live output · o tail -f "
		}
		lblStyle = styleInspectLive
	case moreCount > 0 && ev.DetailKind == "shell":
		label = fmt.Sprintf(" ⋯ %d earlier lines · o inspect ", moreCount)
		lblStyle = styleInspectQuiet
	case moreCount > 0:
		label = fmt.Sprintf(" ⋯ %d more lines · o inspect ", moreCount)
		lblStyle = styleInspectQuiet
	default:
		label = " o inspect full view "
		lblStyle = styleInspectQuiet
	}
	if highlight && ev.Status != "running" {
		lblStyle = styleInspectSel
	}
	lblRendered := lblStyle.Render(label)
	lblW := ansi.StringWidth(lblRendered)
	if lblW > innerW-2 && innerW > 4 {
		lblRendered = clampLineWidth(lblRendered, innerW-2)
		lblW = ansi.StringWidth(lblRendered)
	}
	leftDash := max(1, innerW-lblW-1)
	return styleCodeBorder.Render("╰"+strings.Repeat("─", leftDash)) + lblRendered + styleCodeBorder.Render("─╯")
}

func formatOpToken(op events.OpItem) (plain string, styled string) {
	mark := "✓"
	markStyled := styleBadgeOk.Render("✓")
	capStyled := stylePath.Render(op.Capability)
	if op.Status == "error" {
		mark = "✗"
		markStyled = styleBadgeErr.Render("✗")
		capStyled = styleBadgeErr.Render(op.Capability)
	}
	dur := fmt.Sprintf("(%dms)", op.DurationMs)
	plain = mark + op.Capability + dur
	styled = markStyled + capStyled + styleDim.Render(dur)
	return plain, styled
}

// groupOpsIntoWaves sorts ops by dispatch Seq and groups ops sharing a parallel Wave ID.
func groupOpsIntoWaves(ops []events.OpItem) [][]events.OpItem {
	if len(ops) == 0 {
		return nil
	}
	sorted := make([]events.OpItem, len(ops))
	copy(sorted, ops)
	// Insertion sort preserves original order when Seq == 0.
	for i := 1; i < len(sorted); i++ {
		cur := sorted[i]
		if cur.Seq <= 0 {
			continue
		}
		j := i - 1
		for j >= 0 && sorted[j].Seq > cur.Seq {
			sorted[j+1] = sorted[j]
			j--
		}
		sorted[j+1] = cur
	}

	var waves [][]events.OpItem
	waveIdxByID := map[int]int{}
	for _, op := range sorted {
		if op.Wave > 0 {
			if idx, ok := waveIdxByID[op.Wave]; ok {
				waves[idx] = append(waves[idx], op)
				continue
			}
			waveIdxByID[op.Wave] = len(waves)
			waves = append(waves, []events.OpItem{op})
		} else {
			waves = append(waves, []events.OpItem{op})
		}
	}
	return waves
}

func renderOpsChainInline(ops []events.OpItem) string {
	waves := groupOpsIntoWaves(ops)
	var b strings.Builder
	b.WriteString(styleDim.Render("▸ Ops: "))
	arrow := styleFooterSep.Render(" → ")
	for wi, w := range waves {
		if wi > 0 {
			b.WriteString(arrow)
		}
		if len(w) == 1 {
			_, st := formatOpToken(w[0])
			b.WriteString(st)
			continue
		}
		b.WriteString(styleAccent.Render("‖") + styleDim.Render("["))
		for bi, op := range w {
			if bi > 0 {
				b.WriteString(styleDim.Render(", "))
			}
			_, st := formatOpToken(op)
			b.WriteString(st)
		}
		b.WriteString(styleDim.Render("]"))
	}
	return b.String()
}

// renderOpsChainDetailLines renders a compact wrapped `▸ Ops:` chain for purely sequential runs,
// or a GrokNight Fork-Join parallel flowchart with aligned mini Gantt timing bars when any
// `Promise.all` parallel wave is present.
func renderOpsChainDetailLines(ops []events.OpItem, wrapW int) []detailLine {
	if len(ops) == 0 {
		return nil
	}
	waves := groupOpsIntoWaves(ops)
	parallelWaves := 0
	for _, w := range waves {
		if len(w) > 1 {
			parallelWaves++
		}
	}
	if parallelWaves > 0 {
		return renderParallelFlowchartLines(ops, waves, parallelWaves, wrapW)
	}

	const firstPrefixPlain = "▸ Ops: "
	const contPrefixPlain = "       "
	firstPrefixStyled := styleAccent.Render("▸") + " " + styleModalKey.Render("Ops:") + " "
	contPrefixStyled := contPrefixPlain
	arrowPlain := " → "
	arrowStyled := styleFooterSep.Render(" → ")

	var out []detailLine
	curPlain := firstPrefixPlain
	curStyled := firstPrefixStyled
	curW := ansi.StringWidth(firstPrefixPlain)

	for i, op := range ops {
		tokPlain, tokStyled := formatOpToken(op)
		sepPlain, sepStyled := "", ""
		if i > 0 {
			sepPlain = arrowPlain
			sepStyled = arrowStyled
		}
		addW := ansi.StringWidth(sepPlain + tokPlain)
		if i > 0 && curW+addW > wrapW && curW > ansi.StringWidth(contPrefixPlain) {
			out = append(out, detailLine{plain: curPlain, body: curStyled})
			curPlain = contPrefixPlain + "→ " + tokPlain
			curStyled = contPrefixStyled + styleFooterSep.Render("→ ") + tokStyled
			curW = ansi.StringWidth(curPlain)
			continue
		}
		curPlain += sepPlain + tokPlain
		curStyled += sepStyled + tokStyled
		curW += addW
	}
	if curPlain != "" {
		out = append(out, detailLine{plain: curPlain, body: curStyled})
	}
	return out
}

func renderParallelFlowchartLines(ops []events.OpItem, waves [][]events.OpItem, parallelWaves int, wrapW int) []detailLine {
	waveLbl := "parallel wave"
	if parallelWaves > 1 {
		waveLbl = "parallel waves"
	}
	hdrPlain := fmt.Sprintf("▸ Ops Flow  (%d ops · %d %s)", len(ops), parallelWaves, waveLbl)
	hdrStyled := styleAccent.Render("▸") + " " + styleModalTitle.Render("Ops Flow") + "  " +
		styleDim.Render(fmt.Sprintf("(%d ops · %d %s)", len(ops), parallelWaves, waveLbl))

	out := []detailLine{{plain: hdrPlain, body: hdrStyled}}

	const barW = 12
	for wi, w := range waves {
		isFirst := wi == 0
		isLast := wi == len(waves)-1

		if len(w) == 1 {
			op := w[0]
			var connPlain string
			switch {
			case isFirst && isLast:
				connPlain = "  ● "
			case isFirst:
				connPlain = "  ┌─● "
			case isLast:
				connPlain = "  └─● "
			default:
				connPlain = "  ├─● "
			}
			connStyled := styleTreeConn.Render(connPlain[:len(connPlain)-len("● ")]) + styleAccent.Render("● ")
			tokPlain, tokStyled := formatOpToken(op)
			targetPlain, targetStyled := formatOpTargetHint(op.Target, max(12, wrapW-ansi.StringWidth(connPlain)-ansi.StringWidth(tokPlain)-2))
			out = append(out, detailLine{
				plain: connPlain + tokPlain + targetPlain,
				body:  connStyled + tokStyled + targetStyled,
			})
			if !isLast {
				out = append(out, detailLine{plain: "  │", body: styleTreeConn.Render("  │")})
			}
			continue
		}

		// Parallel wave (Promise.all fork-join block)
		minStart := w[0].StartMs
		maxEnd := w[0].StartMs + w[0].DurationMs
		var sumDur, maxDur int64
		maxLabelW := 0
		for _, op := range w {
			if op.StartMs < minStart {
				minStart = op.StartMs
			}
			if end := op.StartMs + op.DurationMs; end > maxEnd {
				maxEnd = end
			}
			sumDur += op.DurationMs
			if op.DurationMs > maxDur {
				maxDur = op.DurationMs
			}
			tp, _ := formatOpToken(op)
			if lw := ansi.StringWidth(tp); lw > maxLabelW {
				maxLabelW = lw
			}
		}
		if maxLabelW > 28 {
			maxLabelW = 28
		}
		wallMs := maxEnd - minStart
		if wallMs <= 0 {
			wallMs = maxDur
		}
		savedMs := max(int64(0), sumDur-wallMs)

		var forkPlain, forkStyled string
		switch {
		case isFirst && isLast:
			forkPlain = "  ┬─ "
			forkStyled = styleTreeConn.Render("  ┬─ ")
		case isFirst:
			forkPlain = "  ┌─┬─ "
			forkStyled = styleTreeConn.Render("  ┌─┬─ ")
		case isLast:
			forkPlain = "  └─┬─ "
			forkStyled = styleTreeConn.Render("  └─┬─ ")
		default:
			forkPlain = "  ├─┬─ "
			forkStyled = styleTreeConn.Render("  ├─┬─ ")
		}

		timingMeta := fmt.Sprintf("(wall %dms)", wallMs)
		if savedMs > 0 {
			timingMeta = fmt.Sprintf("(wall %dms · saved %dms)", wallMs, savedMs)
		}
		forkHdrPlain := fmt.Sprintf("%s‖ parallel ×%d  %s", forkPlain, len(w), timingMeta)
		forkHdrStyled := forkStyled + styleAccent.Render(fmt.Sprintf("‖ parallel ×%d", len(w))) + "  " + styleDim.Render(timingMeta)
		out = append(out, detailLine{plain: forkHdrPlain, body: forkHdrStyled})

		// Spine prefix for branch lines inside this parallel wave
		spinePlain := "  │ "
		spineStyled := styleTreeConn.Render("  │ ")
		if isLast {
			if isFirst {
				spinePlain = "  "
				spineStyled = "  "
			} else {
				spinePlain = "    "
				spineStyled = "    "
			}
		}

		for bi, op := range w {
			branchConn := "├── "
			if bi == len(w)-1 {
				branchConn = "└── "
			}
			tokPlain, tokStyled := formatOpToken(op)
			tokW := ansi.StringWidth(tokPlain)
			padSpaces := max(1, maxLabelW-tokW+2)
			barPlain, barStyled := renderMiniGanttBar(op, minStart, wallMs, barW)

			prefixW := ansi.StringWidth(spinePlain) + ansi.StringWidth(branchConn) + tokW + padSpaces + barW
			targetPlain, targetStyled := formatOpTargetHint(op.Target, max(0, wrapW-prefixW-2))

			out = append(out, detailLine{
				plain: spinePlain + branchConn + tokPlain + strings.Repeat(" ", padSpaces) + barPlain + targetPlain,
				body:  spineStyled + styleTreeConn.Render(branchConn) + tokStyled + strings.Repeat(" ", padSpaces) + barStyled + targetStyled,
			})
		}

		if !isLast {
			out = append(out, detailLine{plain: "  │", body: styleTreeConn.Render("  │")})
		}
	}
	return out
}

func renderMiniGanttBar(op events.OpItem, waveMinStart, waveSpan int64, width int) (plain string, styled string) {
	if width <= 0 {
		return "", ""
	}
	startCell := 0
	fillCells := width
	if waveSpan > 0 {
		relStart := max(int64(0), op.StartMs-waveMinStart)
		startCell = int((relStart * int64(width)) / waveSpan)
		if startCell >= width {
			startCell = width - 1
		}
		fillCells = int((op.DurationMs*int64(width) + waveSpan - 1) / waveSpan)
		if fillCells < 1 {
			fillCells = 1
		}
		if startCell+fillCells > width {
			fillCells = width - startCell
		}
	}
	endEmpty := max(0, width-startCell-fillCells)
	lead := strings.Repeat("─", startCell)
	fill := strings.Repeat("━", fillCells)
	tail := strings.Repeat("─", endEmpty)
	plain = lead + fill + tail

	fillStyle := styleGanttOk
	if op.Status == "error" {
		fillStyle = styleGanttErr
	}
	styled = styleGanttTrack.Render(lead) + fillStyle.Render(fill) + styleGanttTrack.Render(tail)
	return plain, styled
}

func formatOpTargetHint(target string, maxW int) (plain string, styled string) {
	target = strings.TrimSpace(strings.ReplaceAll(strings.ReplaceAll(target, "\r", " "), "\n", " "))
	if target == "" || maxW < 4 {
		return "", ""
	}
	clamped := clampLineWidth(target, maxW)
	return "  " + clamped, "  " + styleDim.Render(clamped)
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
		sessBadge := m.renderInlineSessionBadge(g.startEv.SessionID)
		left := fmt.Sprintf("%s %s %s %s%s", rail, arrow, dot, sessBadge, title)
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
	sessBadge := m.renderInlineSessionBadge(g.startEv.SessionID)
	left := fmt.Sprintf("%s %s %s %s%s %s",
		rail, arrow, dot,
		sessBadge,
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
	left = sanitizeStyledLine(left)
	right = sanitizeStyledLine(right)
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

func renderEventBodyLines(ev events.Event, pad string, maxW int, hoverCopy bool) ([]string, int, int, int) {
	var out []string
	totalLines := 0
	stripW := max(24, maxW-2)

	if ev.Diff != nil && len(ev.Diff.Hunks) > 0 {
		diffLang := detectLang(ev.Path, "", "", "")
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
					out = append(out, pad+renderColoredDiffLine(dl, stripW, diffLang))
				}
			}
		}
		more := max(0, totalLines-len(out))
		return out, more, 0, 0
	}

	if ev.Error != "" && ev.Detail == "" {
		out = append(out, pad+styleCodeRail.Render("│ ")+styleBadgeErr.Render("Error: "+ev.Error))
		return out, 0, 0, 0
	}
	if ev.Detail == "" && ev.Status == "running" {
		out = append(out, pad+styleCodeRail.Render("│ ")+styleDim.Render("waiting for output…"))
		return out, 0, 0, 0
	}
	if ev.Detail != "" {
		detailText := ev.Detail
		lang := detectLang(ev.Path, ev.DetailKind, ev.Tool, detailText)
		isCode := ev.DetailKind == "code" || ev.Tool == "code"
		isResult := ev.Tool == "result"
		isShell := ev.DetailKind == "shell"
		if isCode {
			detailText = dedentBlock(detailText)
		}
		raw := splitSanitizedLines(detailText)
		totalLines = len(raw)

		// Render framed recessed GrokNight box with language-aware syntax highlighting!
		if isBoxedCardEvent(ev) && stripW >= 30 {
			boxW := stripW
			innerW := boxW - 2
			tag := lang
			tagStyle := styleSynGlobal
			switch {
			case isCode:
				tag = "typescript"
				tagStyle = styleAccent
			case isShell:
				tag = "shell"
				if ev.Kind == events.KindTask {
					tag = "task"
				}
				if ev.Status == "running" {
					tag += " · live"
					tagStyle = styleRunDot
				} else {
					tagStyle = styleOpShell
				}
			case isResult:
				if ev.Status != "" && ev.Status != "ok" {
					tag = "output · error"
					tagStyle = styleBadgeErr
				} else {
					tag = "output"
					tagStyle = styleBadgeOk
				}
			case ev.Tool == "docs":
				tag = "docs · markdown"
				tagStyle = styleAccent
			case lang == "search":
				tag = "search"
				tagStyle = styleOpSearch
			case lang == "text":
				tag = "file"
				tagStyle = styleCodeHeader
			}

			meta := fmt.Sprintf(" %d lines ", totalLines)
			copyBtn := styleModalBtn.Render(" Copy ")
			if hoverCopy {
				copyBtn = styleCtrlHover.Render(" Copy ")
			}
			copyW := ansi.StringWidth(copyBtn)
			leftHdr := styleCodeBorder.Render("─ ") + tagStyle.Render(tag) + " "
			metaRendered := styleDim.Render(meta)
			rightHdr := metaRendered + copyBtn + styleCodeBorder.Render("─")
			copyX1, copyX2 := 0, 0
			hasCopyBtn := true
			if ansi.StringWidth(leftHdr)+ansi.StringWidth(rightHdr)+1 > innerW {
				metaRendered = ""
				rightHdr = copyBtn + styleCodeBorder.Render("─")
				if ansi.StringWidth(leftHdr)+ansi.StringWidth(rightHdr)+1 > innerW {
					hasCopyBtn = false
					rightHdr = styleCodeBorder.Render("─")
				}
			}
			dashCount := max(1, innerW-ansi.StringWidth(leftHdr)-ansi.StringWidth(rightHdr))
			if hasCopyBtn {
				copyX1 = ansi.StringWidth(pad) + 1 + ansi.StringWidth(leftHdr) + dashCount + ansi.StringWidth(metaRendered)
				copyX2 = copyX1 + copyW
			}
			topBorder := styleCodeBorder.Render("╭") + leftHdr + styleCodeBorder.Render(strings.Repeat("─", dashCount)) + rightHdr + styleCodeBorder.Render("╮")
			out = append(out, pad+topBorder)

			startIdx := 0
			endIdx := min(totalLines, maxInlinePreviewLines)
			more := max(0, totalLines-endIdx)
			if isShell && totalLines > maxInlinePreviewLines {
				startIdx = totalLines - maxInlinePreviewLines
				endIdx = totalLines
				more = startIdx
			}

			// Highlight full slice when Markdown (tracks fenced code blocks) or only visible slice otherwise.
			var hlSlice []string
			if lang == "markdown" {
				hlAll := highlightLines(raw[:endIdx], lang)
				hlSlice = hlAll[startIdx:endIdx]
			} else {
				hlSlice = highlightLines(raw[startIdx:endIdx], lang)
			}

			for idx, hl := range hlSlice {
				lineNo := startIdx + idx + 1
				ln := styleCodeLineNum.Render(fmt.Sprintf(" %4d │ ", lineNo))
				rowInner := paintLineBg(ln+hl, colorCodeWell, innerW)
				out = append(out, pad+styleCodeBorder.Render("│")+rowInner+styleCodeBorder.Render("│"))
			}
			return out, more, copyX1, copyX2
		}

		hl := highlightLines(raw[:min(totalLines, maxInlinePreviewLines)], lang)
		for i, hLine := range hl {
			out = append(out, fmt.Sprintf("%s%s%s", pad, styleLineNum.Render(fmt.Sprintf("%4d │ ", i+1)), hLine))
		}
		more := max(0, totalLines-len(out))
		return out, more, 0, 0
	}
	return out, 0, 0, 0
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

func renderColoredDiffLine(dl diff.DiffLine, stripW int, lang string) string {
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
		return styleLineNum.Render(numStr) + "  " + highlightLineByLang(dl.Text, lang)
	}
}

func renderSpannedLine(sign, text string, spans []diff.Span, baseStyle, hiStyle interface{ Render(...string) string }) string {
	if len(spans) == 0 {
		return baseStyle.Render(sign + sanitizePlainLine(text))
	}
	var sb strings.Builder
	sb.WriteString(baseStyle.Render(sign))
	col := 0
	for _, sp := range spans {
		if sp.Start < 0 || sp.End > len(text) || sp.Start >= sp.End {
			continue
		}
		var chunk string
		chunk, col = sanitizePlainChunk(text[sp.Start:sp.End], col)
		if chunk == "" {
			continue
		}
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

type modalVisRow struct {
	logicalIdx     int
	isCont         bool
	gutter         string
	bodyStyled     string
	bodyWidth      int
	bgHex          string
	panGroup       int
	headerForGroup int
	hasText        bool
}

func (m *Model) buildModalDisplayRows(contentW int) []modalVisRow {
	re := compileSearchRegex(m.detail.query)
	curMatchLine := -1
	if len(m.detail.matches) > 0 && m.detail.matchCursor >= 0 && m.detail.matchCursor < len(m.detail.matches) {
		curMatchLine = m.detail.matches[m.detail.matchCursor]
	}

	var out []modalVisRow
	for i, dl := range m.detail.lines {
		gutter := dl.gutter
		body := dl.body
		if m.detail.query != "" && re != nil && re.MatchString(dl.plain) {
			hi := styleSearchMatch
			if i == curMatchLine {
				hi = styleSearchCur
			}
			if gw := ansi.StringWidth(gutter); gw > 0 {
				gutter = hi.Render("›") + ansi.Cut(gutter, 1, gw)
			}
			body = highlightPlainQuery(dl.plain, re, hi)
		}

		hasTxt := strings.TrimSpace(dl.plain) != ""
		bodyW := ansi.StringWidth(body)
		if !m.detail.wrap {
			out = append(out, modalVisRow{
				logicalIdx:     i,
				gutter:         gutter,
				bodyStyled:     body,
				bodyWidth:      bodyW,
				bgHex:          dl.bgHex,
				panGroup:       dl.panGroup,
				headerForGroup: dl.headerForGroup,
				hasText:        hasTxt,
			})
			continue
		}

		gutterW := ansi.StringWidth(gutter)
		availBodyW := max(8, contentW-gutterW-1) // reserve 1 col for cursor rail
		if bodyW <= availBodyW {
			out = append(out, modalVisRow{
				logicalIdx:     i,
				gutter:         gutter,
				bodyStyled:     body,
				bodyWidth:      bodyW,
				bgHex:          dl.bgHex,
				panGroup:       dl.panGroup,
				headerForGroup: dl.headerForGroup,
				hasText:        hasTxt,
			})
			continue
		}

		contGutter := buildContinuationGutter(gutterW, dl.bgHex)
		for off := 0; off < bodyW; {
			seg, step := cutWrapSegment(body, off, availBodyW)
			g := gutter
			if off > 0 {
				g = contGutter
			}
			out = append(out, modalVisRow{
				logicalIdx:     i,
				isCont:         off > 0,
				gutter:         g,
				bodyStyled:     seg,
				bodyWidth:      ansi.StringWidth(seg),
				bgHex:          dl.bgHex,
				panGroup:       dl.panGroup,
				headerForGroup: dl.headerForGroup,
				hasText:        hasTxt,
			})
			off += step
		}
	}
	return out
}

// cutWrapSegment slices a soft-wrapped chunk of at most maxCols starting at exact rune boundary off,
// returning the styled segment and the exact column step to advance off without splitting 2-cell CJK runes.
func cutWrapSegment(styled string, off, maxCols int) (string, int) {
	if maxCols <= 0 {
		return "", 1
	}
	seg := ansi.Cut(styled, off, off+maxCols)
	if ansi.StringWidth(seg) > maxCols {
		seg = ansi.Truncate(seg, maxCols, "")
	}
	w := ansi.StringWidth(seg)
	if w <= 0 {
		return "", 1
	}
	return seg, w
}

// cutStyledSpan extracts display columns [startCol, startCol+maxCols) for horizontal pan,
// replacing a split 2-cell wide CJK rune on the left boundary with a single space and strictly
// bounding the right boundary to <= maxCols so '$' end-pan never overflows into "…" truncation.
func cutStyledSpan(styled string, startCol, maxCols int) string {
	if maxCols <= 0 || styled == "" {
		return ""
	}
	leadPad := ""
	if startCol > 0 {
		leftW := ansi.StringWidth(ansi.Cut(styled, 0, startCol))
		if leftW < startCol {
			leadPad = " "
			startCol++
			maxCols--
			if maxCols <= 0 {
				return leadPad
			}
		}
	}
	seg := ansi.Cut(styled, startCol, startCol+maxCols)
	if ansi.StringWidth(seg) > maxCols {
		seg = ansi.Truncate(seg, maxCols, "")
	}
	return leadPad + seg
}

func buildContinuationGutter(gutterW int, bgHex string) string {
	if gutterW <= 0 {
		return ""
	}
	raw := strings.Repeat(" ", gutterW)
	if gutterW >= 4 {
		raw = strings.Repeat(" ", gutterW-4) + "· │ "
	}
	switch bgHex {
	case colorDiffAddBg:
		return styleDiffAddNum.Render(raw)
	case colorDiffDelBg:
		return styleDiffDelNum.Render(raw)
	default:
		return styleLineNum.Render(raw)
	}
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

	// Row 3: Subtle hairline divider (highlights lavender when body cursor is focused)
	divStyle := styleDividerModal
	if m.detail.focus == modalFocusBody && !m.detail.searchMode {
		divStyle = styleDividerFocused
	}
	divRow := bdr.Render("├") + paintLineBg(divStyle.Render(strings.Repeat("─", innerW)), colorSurface, innerW) + bdr.Render("┤")

	// Scrollable body rows with pinned gutter, active line cursor, horizontal pan, or soft word-wrap
	page := max(3, mh-7)
	contentW := max(10, innerW-2) // 1 col cursor rail + (contentW) content + 1 col scrollbar
	visRows := m.buildModalDisplayRows(contentW)
	totalVis := len(visRows)
	m.detail.totalVis = totalVis

	maxScroll := max(0, totalVis-page)
	if m.detail.scroll > maxScroll {
		m.detail.scroll = maxScroll
	}
	if totalVis == 0 {
		m.detail.cursor = 0
	} else if m.detail.cursor >= totalVis {
		m.detail.cursor = totalVis - 1
	}

	// Precompute per-section horizontal pan bounds & clamped offsets (when wrap == false).
	groupMaxH := map[int]int{}
	groupH := map[int]int{}
	if !m.detail.wrap {
		for _, vr := range visRows {
			if vr.panGroup <= 0 {
				continue
			}
			gw := ansi.StringWidth(vr.gutter)
			avail := max(4, contentW-gw)
			if over := vr.bodyWidth - avail; over > groupMaxH[vr.panGroup] {
				groupMaxH[vr.panGroup] = over
			}
		}
		for grp, maxH := range groupMaxH {
			if h := m.detailGroupHScroll(grp); h > 0 {
				groupH[grp] = min(h, maxH)
			}
		}
	}

	thumbStart, thumbEnd := 0, page
	if totalVis > page && page > 0 {
		thumbSize := max(1, (page*page)/totalVis)
		thumbStart = (m.detail.scroll * (page - thumbSize)) / max(1, maxScroll)
		thumbEnd = min(page, thumbStart+thumbSize)
	}

	m.detail.copyZones = nil
	bodyRows := make([]string, page)
	for i := 0; i < page; i++ {
		visIdx := m.detail.scroll + i
		var rowContent string
		rowBg := colorSurface

		if visIdx < totalVis {
			vr := visRows[visIdx]
			isCursor := visIdx == m.detail.cursor
			isHover := visIdx == m.detail.hoverRow && !isCursor

			if vr.bgHex == colorDiffAddBg || vr.bgHex == colorDiffDelBg {
				rowBg = vr.bgHex
			} else if isCursor {
				rowBg = colorSurfaceSel
			} else if isHover {
				rowBg = colorHoverRow
			} else if vr.bgHex != "" {
				rowBg = vr.bgHex
			}

			rail := " "
			if isCursor {
				if m.detail.focus == modalFocusBody {
					rail = styleSelectedBar.Render("▎")
				} else {
					rail = styleDim.Render("│")
				}
			}

			gutterW := ansi.StringWidth(vr.gutter)
			availBodyW := max(4, contentW-gutterW)

			bodySeg := vr.bodyStyled
			if !m.detail.wrap {
				// Block-scoped horizontal scroll: all lines sharing a panGroup (e.g. CODE, OUTPUT, diff/file body)
				// pan together in lockstep while Ops Flow, section headers, and other sections stay stationary.
				if vr.panGroup > 0 {
					rowHScroll := groupH[vr.panGroup]
					overflowRight := vr.bodyWidth > rowHScroll+availBodyW
					cutW := availBodyW
					if overflowRight && availBodyW > 1 {
						cutW = availBodyW - 1
					}
					cutSeg := cutStyledSpan(vr.bodyStyled, rowHScroll, cutW)
					if overflowRight && availBodyW > 1 {
						padW := max(0, cutW-ansi.StringWidth(cutSeg))
						rightMark := styleDim.Render("▸")
						if isCursor {
							rightMark = styleAccent.Render("▸")
						}
						bodySeg = cutSeg + strings.Repeat(" ", padW) + rightMark
					} else {
						bodySeg = cutSeg
					}
				} else if vr.headerForGroup == 0 {
					// Stationary row (Ops Flow, hunk separator)
					if ansi.StringWidth(vr.bodyStyled) > availBodyW && availBodyW > 1 {
						cutSeg := cutStyledSpan(vr.bodyStyled, 0, availBodyW-1)
						padW := max(0, (availBodyW-1)-ansi.StringWidth(cutSeg))
						bodySeg = cutSeg + strings.Repeat(" ", padW) + styleDim.Render("▸")
					}
				}
			}
			if vr.headerForGroup > 0 {
				// Section header (CODE / OUTPUT): always stationary with right-aligned clickable [ Copy ] pill!
				hdrStyled := vr.bodyStyled
				if !m.detail.wrap {
					if gh := groupH[vr.headerForGroup]; gh > 0 {
						hdrStyled += "  " + styleAccent.Render(fmt.Sprintf("◂ +%d", gh))
					} else if groupMaxH[vr.headerForGroup] > 0 {
						hdrStyled += "  " + styleDim.Render("←→")
					}
				}
				copyBtn := styleModalBtn.Render(" Copy ")
				if m.detail.hoverCopyGroup == vr.headerForGroup {
					copyBtn = styleCtrlHover.Render(" Copy ")
				}
				btnW := ansi.StringWidth(copyBtn)
				availForLeft := availBodyW - btnW - 2
				if availForLeft >= 10 {
					leftClamped := clampLineWidth(hdrStyled, availForLeft)
					leftW := ansi.StringWidth(leftClamped)
					gap := max(1, availBodyW-leftW-btnW-1)
					bodySeg = leftClamped + strings.Repeat(" ", gap) + copyBtn + " "
					btnX := mx + 2 + gutterW + leftW + gap
					m.detail.copyZones = append(m.detail.copyZones, modalCopyZone{
						y:     my + 4 + i,
						x1:    btnX,
						x2:    btnX + btnW,
						group: vr.headerForGroup,
					})
				} else {
					bodySeg = clampLineWidth(hdrStyled, availBodyW)
				}
			}
			if m.sel.active && m.sel.scope == selScopeModal {
				r1, c1, r2, c2 := m.sel.normalized()
				if visIdx >= r1 && visIdx <= r2 {
					if m.sel.lineMode {
						rowBg = colorTextSelBg
					} else {
						rowHScroll := 0
						if !m.detail.wrap && vr.panGroup > 0 {
							rowHScroll = groupH[vr.panGroup]
						}
						segW := ansi.StringWidth(bodySeg)
						visStart := 0
						visEnd := segW
						if visIdx == r1 {
							visStart = max(0, c1-rowHScroll)
						}
						if visIdx == r2 {
							visEnd = max(0, (c2+1)-rowHScroll)
						}
						bodySeg = applySelectionHighlight(bodySeg, segW, visStart, visEnd)
					}
				}
			}
			rowContent = rail + vr.gutter + bodySeg
		}

		sbChar := styleModalScrollTrack.Render("│")
		if totalVis > page && i >= thumbStart && i < thumbEnd {
			sbChar = styleModalScrollThumb.Render("┃")
		} else if totalVis <= page {
			sbChar = paintLineBg(" ", colorSurface, 1)
		}
		bodyRows[i] = bdr.Render("│") + paintLineBg(rowContent, rowBg, contentW+1) + sbChar + bdr.Render("│")
	}

	// Bottom divider highlights when bottom action bar is focused
	botDivStyle := styleDividerModal
	if m.detail.focus == modalFocusBar && !m.detail.searchMode {
		botDivStyle = styleDividerFocused
	}
	botDivRow := bdr.Render("├") + paintLineBg(botDivStyle.Render(strings.Repeat("─", innerW)), colorSurface, innerW) + bdr.Render("┤")

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

		wrapLbl := "Wrap:OFF"
		if m.detail.wrap {
			wrapLbl = "Wrap:ON"
		}
		btns := []string{"/ Find", "▲", "▼", wrapLbl, "Copy", "Top", "End"}
		for idx, lbl := range btns {
			focused := m.detail.focus == modalFocusBar && idx == m.detail.btnIdx
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
			case idx == 3 && m.detail.wrap:
				rendered = styleFollowOn.Render(txt)
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

		// Right-aligned live status + copy toast + visual mode + cursor line + horizontal pan offset + %
		var statusParts []string
		if m.sel.active && m.sel.scope == selScopeModal && m.sel.lineMode {
			r1, _, r2, _ := m.sel.normalized()
			statusParts = append(statusParts, styleAccent.Render(fmt.Sprintf("VISUAL %dL (y)", r2-r1+1)))
		} else if m.copyToast != "" {
			ts := styleCopyToast
			if m.copyToastErr {
				ts = styleCopyToastErr
			}
			statusParts = append(statusParts, ts.Render(" "+m.copyToast+" "))
		}
		if m.detail.streaming {
			if m.detail.followTail {
				statusParts = append(statusParts, styleRunDot.Render(m.currentSpinner()+" tail -f"))
			} else {
				statusParts = append(statusParts, styleDim.Render("paused (G)"))
			}
		}
		if !m.detail.wrap {
			if activeH := groupH[m.detailActivePanGroup(visRows)]; activeH > 0 {
				statusParts = append(statusParts, styleAccent.Render(fmt.Sprintf("◂ Col+%d", activeH)))
			}
		}
		curLn := 0
		if totalVis > 0 && m.detail.cursor < totalVis {
			curLn = visRows[m.detail.cursor].logicalIdx + 1
		}
		endRow := min(totalVis, m.detail.scroll+page)
		pct := 100
		if totalVis > 0 {
			pct = min(100, (endRow*100)/totalVis)
		}
		statusParts = append(statusParts, styleModalMeta.Render(fmt.Sprintf("Ln %d/%d  %d%% ", curLn, len(m.detail.lines), pct)))

		// Gracefully drop lower-priority trailing status segments if narrow modal lacks width so VISUAL / Copy toast always show.
		availRight := innerW - (col - (mx + 1))
		posInfo := ""
		for k := len(statusParts); k >= 1; k-- {
			cand := strings.Join(statusParts[:k], styleFooterSep.Render(" · "))
			if k < len(statusParts) {
				cand += " "
			}
			if ansi.StringWidth(cand) < availRight {
				posInfo = cand
				break
			}
		}
		if posInfo == "" && len(statusParts) > 0 && availRight > 6 {
			posInfo = clampLineWidth(statusParts[0], availRight-2) + " "
		}
		if posInfo != "" {
			gap := max(1, availRight-ansi.StringWidth(posInfo))
			ab.WriteString(strings.Repeat(" ", gap))
			ab.WriteString(posInfo)
		}
		actionRow = bdr.Render("│") + paintLineBg(ab.String(), colorSurfaceHi, innerW) + bdr.Render("│")
	}

	bottomRow := bdr.Render("╰" + strings.Repeat("─", innerW) + "╯")

	var box []string
	box = append(box, topRow, row1, row2, divRow)
	box = append(box, bodyRows...)
	box = append(box, botDivRow, actionRow, bottomRow)
	return box
}

func (m *Model) renderInlineSessionBadge(sid string) string {
	// Show compact color-coded client badge on feed headers when viewing All Sessions across multiple clients.
	if m.activeSessionID != "" || len(m.sessions) < 2 || sid == "" {
		return ""
	}
	info := m.sessionByID[sid]
	st := styleAccent
	if info != nil {
		st = sessionAccentStyles[info.colorIdx%len(sessionAccentStyles)]
	}
	name := m.sessionDisplayName(sid)
	if len([]rune(name)) > 14 {
		name = string([]rune(name)[:13]) + "…"
	}
	return st.Render("["+name+"]") + " "
}

// compositeSessionPickerModal overlays a sleek GrokNight rounded session switcher popup.
func (m *Model) compositeSessionPickerModal(baseView string) string {
	mx, my, mw, mh := m.computeSessionPickerBounds()
	m.sessPicker.rect = modalRect{x: mx, y: my, w: mw, h: mh}

	popupLines := m.buildSessionPickerBox(mx, my, mw, mh)
	baseLines := strings.Split(baseView, "\n")
	for len(baseLines) < m.height {
		baseLines = append(baseLines, "")
	}

	out := make([]string, len(baseLines))
	for y := 0; y < len(baseLines); y++ {
		plain := ansi.Strip(baseLines[y])
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

func (m *Model) computeSessionPickerBounds() (x, y, w, h int) {
	w = min(m.width-6, max(58, (m.width*64)/100))
	if w > m.width-2 {
		w = max(36, m.width-2)
	}
	rowCount := len(m.sessions) + 1 // All Sessions + each session
	// Overhead: top border(1) + subtitle(1) + divider(1) + rows + divider(1) + footer hint(1) + bottom border(1) = 6
	desiredH := rowCount + 6
	maxH := max(9, m.height-6)
	h = min(maxH, max(9, desiredH))
	x = max(1, (m.width-w)/2)
	y = max(1, (m.height-h)/2)
	return x, y, w, h
}

func (m *Model) buildSessionPickerBox(mx, my, mw, mh int) []string {
	innerW := max(20, mw-2)
	bTop := styleModalBorderTop
	bdr := styleModalBorder

	// Row 0: Top border with title & [ esc × ]
	titlePrefix := " " + styleBrandMark.Render("◆") + " "
	badge := "  " + styleModalBadge.Render(fmt.Sprintf(" %d active ", len(m.sessions)))
	closeTxt := " esc × "
	closeRendered := styleModalClose.Render(closeTxt)
	if m.sessPicker.hoverIdx == -2 {
		closeRendered = styleModalCloseHover.Render(closeTxt)
	}
	closeW := ansi.StringWidth(closeRendered)
	maxTitleW := max(4, innerW-closeW-3)
	rawTitle := titlePrefix + styleModalTitle.Render("Sessions") + badge + " "
	titleCombined := paintLineBg(rawTitle, colorSurface, min(maxTitleW, ansi.StringWidth(rawTitle)))
	titleW := ansi.StringWidth(titleCombined)
	fillDash := max(1, innerW-1-titleW-closeW-1)
	closeStartX := mx + 1 + 1 + titleW + fillDash
	m.sessPicker.closeZone = hitZone{x1: closeStartX, x2: closeStartX + closeW, action: "close"}
	topRow := bTop.Render("╭─") + titleCombined + bTop.Render(strings.Repeat("─", fillDash)) + closeRendered + bTop.Render("─╮")

	// Row 1: Subtitle strip explaining what MCPlex sessions are
	subContent := "  " + styleModalMeta.Render("Each MCP client connection gets an isolated session (state, tasks & feed filter)")
	subRow := bdr.Render("│") + paintLineBg(subContent, colorSurfaceHi, innerW) + bdr.Render("│")

	// Row 2: Divider
	divRow := bdr.Render("├") + paintLineBg(styleDividerModal.Render(strings.Repeat("─", innerW)), colorSurface, innerW) + bdr.Render("┤")

	// Body rows (All Sessions + each observed session)
	bodyH := max(2, mh-6)
	m.sessPicker.rowZones = nil

	totalItems := len(m.sessions) + 1
	scrollStart := 0
	if m.sessPicker.cursor >= bodyH {
		scrollStart = m.sessPicker.cursor - bodyH + 1
	}

	bodyRows := make([]string, bodyH)
	for i := 0; i < bodyH; i++ {
		idx := scrollStart + i
		if idx >= totalItems {
			bodyRows[i] = bdr.Render("│") + paintLineBg("", colorSurface, innerW) + bdr.Render("│")
			continue
		}
		screenY := my + 3 + i
		m.sessPicker.rowZones = append(m.sessPicker.rowZones, pickerRowZone{y: screenY, idx: idx})

		focused := idx == m.sessPicker.cursor
		hovered := idx == m.sessPicker.hoverIdx && !focused
		rowBg := colorSurface
		rail := "  "
		if focused {
			rowBg = colorSurfaceSel
			rail = styleSelectedBar.Render("▎ ")
		} else if hovered {
			rowBg = colorHoverRow
		}

		var left, right string
		if idx == 0 {
			check := "  "
			if m.activeSessionID == "" {
				check = styleBadgeOk.Render("✓ ")
			}
			left = rail + check + styleBrandMark.Render("●") + " " + styleGroupText.Render("All Sessions") + "  " + styleDim.Render("0")
			right = styleMuted.Render(fmt.Sprintf("%d sessions · %d runs · %d edits ", len(m.sessions), m.totalRuns, m.totalCRUD))
		} else {
			s := m.sessions[idx-1]
			check := "  "
			if m.activeSessionID == s.id {
				check = styleBadgeOk.Render("✓ ")
			}
			accent := sessionAccentStyles[s.colorIdx%len(sessionAccentStyles)]
			dot := accent.Render("●")
			if s.active > 0 {
				dot = styleRunDot.Render(m.currentSpinner())
			}
			namePart := styleGroupText.Render(s.label) + " " + styleDim.Render("#"+shortSessionID(s.id))
			if s.label == "" {
				namePart = styleGroupText.Render("Session") + " " + styleMuted.Render("#"+shortSessionID(s.id))
			}
			keyHint := ""
			if idx <= 9 {
				keyHint = "  " + styleDim.Render(fmt.Sprintf("%d", idx))
			}
			left = rail + check + dot + " " + namePart + keyHint

			var parts []string
			if s.cwd != "" {
				base := s.cwd
				if slash := strings.LastIndex(strings.TrimRight(base, "/"), "/"); slash >= 0 {
					base = base[slash+1:]
				}
				if base != "" {
					parts = append(parts, styleDim.Render(base))
				}
			}
			parts = append(parts, styleMuted.Render(fmt.Sprintf("%dr/%de", s.runs, s.crud)))
			if s.tasks > 0 {
				parts = append(parts, styleOpTask.Render(fmt.Sprintf("%dt", s.tasks)))
			}
			if s.lastTs != "" {
				parts = append(parts, styleDim.Render(s.lastTs))
			}
			right = strings.Join(parts, styleFooterSep.Render(" · ")) + " "
		}
		bodyRows[i] = bdr.Render("│") + composeSplitRow(left, right, rowBg, innerW) + bdr.Render("│")
	}

	// Footer hint bar
	sep := styleFooterSep.Render("  ·  ")
	hint := func(k, d string) string { return styleFooterKey.Render(k) + " " + styleFooterDesc.Render(d) }
	footContent := "  " + hint("↑↓", "choose") + sep + hint("enter / click", "filter") + sep + hint("0-9", "quick pick") + sep + hint("esc", "close")
	footRow := bdr.Render("│") + paintLineBg(footContent, colorSurfaceHi, innerW) + bdr.Render("│")
	bottomRow := bdr.Render("╰" + strings.Repeat("─", innerW) + "╯")

	var box []string
	box = append(box, topRow, subRow, divRow)
	box = append(box, bodyRows...)
	box = append(box, divRow, footRow, bottomRow)
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
