// Package tui implements a Grok-Build-CLI (GrokNight) inspired dark interactive,
// mouse-driven terminal UI with floating overlay modals for MCPlex CRUD diffs and live logs.
package tui

import (
	"strings"

	"github.com/charmbracelet/lipgloss"
	"github.com/charmbracelet/x/ansi"
)

// GrokNight neutral obsidian-charcoal palette with restrained lavender-magenta accent.
const (
	colorCanvas     = "#0C0D10" // deep neutral obsidian canvas
	colorSurface    = "#13151A" // top bar, footer, floating modal body
	colorSurfaceHi  = "#181B22" // modal header strip, hover subtle fill
	colorSurfaceSel = "#1D212B" // selected feed row fill
	colorHoverRow   = "#151820" // hovered feed row fill
	colorBackdrop   = "#07080A" // dimmed modal scrim
	colorShadow     = "#050608" // subtle modal drop shadow
	colorDiffAddBg  = "#0F2419" // restrained dark emerald diff strip
	colorDiffDelBg  = "#291318" // restrained dark crimson diff strip
)

var (
	// Top bar & status elements (neutral charcoal #13151A)
	styleHeaderBar = lipgloss.NewStyle().
			Foreground(lipgloss.Color("#8B92A3"))

	styleBrandMark = lipgloss.NewStyle().
			Foreground(lipgloss.Color("#D88BFF")).
			Bold(true)

	styleBrand = lipgloss.NewStyle().
			Foreground(lipgloss.Color("#F3F4F6")).
			Bold(true)

	styleLiveDot = lipgloss.NewStyle().
			Foreground(lipgloss.Color("#4ADE80")).
			Bold(true)

	styleRunDot = lipgloss.NewStyle().
			Foreground(lipgloss.Color("#D88BFF")).
			Bold(true)

	styleErrDot = lipgloss.NewStyle().
			Foreground(lipgloss.Color("#F87171")).
			Bold(true)

	// Toolbar controls — clean typographic hierarchy, minimal pills
	styleTabActiveUnfocused = lipgloss.NewStyle().
				Background(lipgloss.Color("#1E222C")).
				Foreground(lipgloss.Color("#F3F4F6")).
				Bold(true)

	styleCtrlFocused = lipgloss.NewStyle().
				Background(lipgloss.Color("#ECEEF2")).
				Foreground(lipgloss.Color("#0C0D10")).
				Bold(true)

	styleCtrlHover = lipgloss.NewStyle().
			Background(lipgloss.Color("#1D212B")).
			Foreground(lipgloss.Color("#ECEEF2"))

	styleCtrlNormal = lipgloss.NewStyle().
			Foreground(lipgloss.Color("#7B8294"))

	styleFollowOn = lipgloss.NewStyle().
			Background(lipgloss.Color("#0F281C")).
			Foreground(lipgloss.Color("#4ADE80")).
			Bold(true)

	// Hairline dividers & structural chrome
	styleDivider        = lipgloss.NewStyle().Foreground(lipgloss.Color("#222631"))
	styleDividerFocused = lipgloss.NewStyle().Foreground(lipgloss.Color("#D88BFF"))
	styleDividerModal   = lipgloss.NewStyle().Foreground(lipgloss.Color("#252936"))

	// Footer keycap bar
	styleFooterKey  = lipgloss.NewStyle().Foreground(lipgloss.Color("#D4D8E3")).Bold(true)
	styleFooterDesc = lipgloss.NewStyle().Foreground(lipgloss.Color("#636A7B"))
	styleFooterSep  = lipgloss.NewStyle().Foreground(lipgloss.Color("#2D3240"))
	styleFooterTag  = lipgloss.NewStyle().Foreground(lipgloss.Color("#D88BFF")).Bold(true)

	// Feed typography & hierarchy
	styleDim         = lipgloss.NewStyle().Foreground(lipgloss.Color("#636A7B"))
	styleMuted       = lipgloss.NewStyle().Foreground(lipgloss.Color("#8B92A3"))
	styleTreeConn    = lipgloss.NewStyle().Foreground(lipgloss.Color("#2E3342"))
	styleCodeRail    = lipgloss.NewStyle().Foreground(lipgloss.Color("#2B3040"))
	styleLineNum     = lipgloss.NewStyle().Foreground(lipgloss.Color("#4C5366"))
	styleHunkSep     = lipgloss.NewStyle().Foreground(lipgloss.Color("#9E8CFF"))
	stylePath        = lipgloss.NewStyle().Foreground(lipgloss.Color("#E5E8F0")).Bold(true)
	styleGroupText   = lipgloss.NewStyle().Foreground(lipgloss.Color("#F3F4F6")).Bold(true)
	styleSelectedBar = lipgloss.NewStyle().Foreground(lipgloss.Color("#D88BFF")).Bold(true)
	styleArrow       = lipgloss.NewStyle().Foreground(lipgloss.Color("#5A6173"))
	styleArrowSel    = lipgloss.NewStyle().Foreground(lipgloss.Color("#D88BFF")).Bold(true)

	// Restrained semantic accents (GrokNight balanced saturation)
	styleOpCreate = lipgloss.NewStyle().Foreground(lipgloss.Color("#4ADE80")).Bold(true)
	styleOpUpdate = lipgloss.NewStyle().Foreground(lipgloss.Color("#7DD3FC")).Bold(true)
	styleOpDelete = lipgloss.NewStyle().Foreground(lipgloss.Color("#F87171")).Bold(true)
	styleOpRename = lipgloss.NewStyle().Foreground(lipgloss.Color("#D88BFF")).Bold(true)
	styleOpRead   = lipgloss.NewStyle().Foreground(lipgloss.Color("#93C5FD")).Bold(true)
	styleOpSearch = lipgloss.NewStyle().Foreground(lipgloss.Color("#FBBF24")).Bold(true)
	styleOpShell  = lipgloss.NewStyle().Foreground(lipgloss.Color("#FB923C")).Bold(true)
	styleOpTask   = lipgloss.NewStyle().Foreground(lipgloss.Color("#34D399")).Bold(true)
	styleOpCode   = lipgloss.NewStyle().Foreground(lipgloss.Color("#8B92A3")).Bold(true)
	styleBadgeOk  = lipgloss.NewStyle().Foreground(lipgloss.Color("#4ADE80")).Bold(true)
	styleBadgeErr = lipgloss.NewStyle().Foreground(lipgloss.Color("#F87171")).Bold(true)
	styleBadgeWrn = lipgloss.NewStyle().Foreground(lipgloss.Color("#FBBF24")).Bold(true)
	styleAccent   = lipgloss.NewStyle().Foreground(lipgloss.Color("#D88BFF")).Bold(true)

	// TrueColor Dark Diff lines + crisp intraline word highlights
	styleDiffAddBg = lipgloss.NewStyle().
			Background(lipgloss.Color(colorDiffAddBg)).
			Foreground(lipgloss.Color("#6EE7A0"))
	styleDiffAddHi = lipgloss.NewStyle().
			Background(lipgloss.Color("#1C5B38")).
			Foreground(lipgloss.Color("#FFFFFF")).
			Bold(true)
	styleDiffDelBg = lipgloss.NewStyle().
			Background(lipgloss.Color(colorDiffDelBg)).
			Foreground(lipgloss.Color("#FCA5A5"))
	styleDiffDelHi = lipgloss.NewStyle().
			Background(lipgloss.Color("#78232F")).
			Foreground(lipgloss.Color("#FFFFFF")).
			Bold(true)
	styleDiffAddNum = lipgloss.NewStyle().
			Background(lipgloss.Color(colorDiffAddBg)).
			Foreground(lipgloss.Color("#3D8A5E"))
	styleDiffDelNum = lipgloss.NewStyle().
			Background(lipgloss.Color(colorDiffDelBg)).
			Foreground(lipgloss.Color("#9C4853"))
	styleDiffCtx = lipgloss.NewStyle().Foreground(lipgloss.Color("#C7CCD8"))

	// Quiet understated inspect link (brightens cleanly on selection/live)
	styleInspectQuiet = lipgloss.NewStyle().Foreground(lipgloss.Color("#5F6A82"))
	styleInspectLive  = lipgloss.NewStyle().Foreground(lipgloss.Color("#D88BFF"))
	styleInspectSel   = lipgloss.NewStyle().Foreground(lipgloss.Color("#93C5FD"))

	// Scrollbars (feed & modal)
	styleScrollThumb = lipgloss.NewStyle().Foreground(lipgloss.Color("#4B5368"))
	styleScrollTrack = lipgloss.NewStyle().Foreground(lipgloss.Color("#1C1F29"))

	// Floating Overlay Modal (GrokNight elevated card over obsidian scrim)
	styleBackdrop = lipgloss.NewStyle().
			Background(lipgloss.Color(colorBackdrop)).
			Foreground(lipgloss.Color("#252934"))

	styleShadow = lipgloss.NewStyle().
			Background(lipgloss.Color(colorShadow)).
			Foreground(lipgloss.Color(colorShadow))

	styleModalBorder = lipgloss.NewStyle().
				Background(lipgloss.Color(colorSurface)).
				Foreground(lipgloss.Color("#323746"))

	styleModalBorderTop = lipgloss.NewStyle().
				Background(lipgloss.Color(colorSurface)).
				Foreground(lipgloss.Color("#454C61"))

	styleModalTitle = lipgloss.NewStyle().
			Background(lipgloss.Color(colorSurface)).
			Foreground(lipgloss.Color("#F3F4F6")).
			Bold(true)

	styleModalBadge = lipgloss.NewStyle().
			Background(lipgloss.Color("#1E222D")).
			Foreground(lipgloss.Color("#B4BCD0"))

	styleModalKey = lipgloss.NewStyle().
			Foreground(lipgloss.Color("#6E7588")).
			Bold(true)

	styleModalPath = lipgloss.NewStyle().
			Foreground(lipgloss.Color("#E5E8F0")).
			Bold(true)

	styleModalMeta = lipgloss.NewStyle().
			Foreground(lipgloss.Color("#8B92A3"))

	styleModalBtn = lipgloss.NewStyle().
			Background(lipgloss.Color("#1E222D")).
			Foreground(lipgloss.Color("#C7CCD8"))

	styleModalClose = lipgloss.NewStyle().
			Background(lipgloss.Color("#1E222D")).
			Foreground(lipgloss.Color("#9AA1B2"))

	styleModalCloseHover = lipgloss.NewStyle().
				Background(lipgloss.Color("#DC2626")).
				Foreground(lipgloss.Color("#FFFFFF")).
				Bold(true)

	styleModalScrollThumb = lipgloss.NewStyle().
				Background(lipgloss.Color(colorSurface)).
				Foreground(lipgloss.Color("#636D87"))

	styleModalScrollTrack = lipgloss.NewStyle().
				Background(lipgloss.Color(colorSurface)).
				Foreground(lipgloss.Color("#1E222D"))

	styleSearchMatch = lipgloss.NewStyle().
				Background(lipgloss.Color("#7C4D00")).
				Foreground(lipgloss.Color("#FFFFFF")).
				Bold(true)
	styleSearchCur = lipgloss.NewStyle().
			Background(lipgloss.Color("#EAB308")).
			Foreground(lipgloss.Color("#0C0D10")).
			Bold(true)

	// Empty-state landing card border
	styleCardBorder = lipgloss.NewStyle().Foreground(lipgloss.Color("#252936"))
)

// bgEscapeSeq asks lipgloss for the exact profile-appropriate ANSI background SGR prefix
// (TrueColor / 256-color / empty under NO_COLOR) so nested ANSI resets never punch holes in surfaces.
func bgEscapeSeq(hex string) string {
	sample := lipgloss.NewStyle().Background(lipgloss.Color(hex)).Render("X")
	idx := strings.IndexByte(sample, 'X')
	if idx <= 0 {
		return ""
	}
	return sample[:idx]
}

// paintLineBg renders styled onto a solid edge-to-edge background of exact width,
// re-applying bgHex immediately after every inner ANSI reset (\x1b[0m / \x1b[m]).
func paintLineBg(styled string, bgHex string, width int) string {
	if width <= 0 {
		return ""
	}
	clamped := clampLineWidth(styled, width)
	pad := max(0, width-ansi.StringWidth(clamped))
	bg := bgEscapeSeq(bgHex)
	if bg == "" {
		return clamped + strings.Repeat(" ", pad)
	}
	filled := strings.ReplaceAll(clamped, "\x1b[0m", "\x1b[0m"+bg)
	filled = strings.ReplaceAll(filled, "\x1b[m", "\x1b[m"+bg)
	return bg + filled + strings.Repeat(" ", pad) + "\x1b[0m"
}

// paintDiffStrip tints a full diff line strip across exact width without leaking background outside it.
func paintDiffStrip(styled string, bgHex string, width int) string {
	return paintLineBg(styled, bgHex, width)
}
