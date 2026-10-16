package tui

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"

	tea "github.com/charmbracelet/bubbletea"
	"github.com/charmbracelet/lipgloss"

	"chatgpt-validator/pkg/validator"
)

type (
	resultMsg  validator.Result
	allDoneMsg struct{}
	tickMsg    struct{}
)

var spinnerFrames = []string{"⠋", "⠙", "⠹", "⠸", "⠼", "⠴", "⠦", "⠧", "⠇", "⠏"}

const (
	recentAllSize    = 10 // top waterfall: rolling window of all results
	recentValidsSize = 6  // bottom waterfall: rolling window of valid results only
)

// Palette & Lipgloss Styles
var (
	colGreen   = lipgloss.Color("10")
	colRed     = lipgloss.Color("9")
	colOrange  = lipgloss.Color("208")
	colYellow  = lipgloss.Color("11")
	colCyan    = lipgloss.Color("14")
	colMagenta = lipgloss.Color("213")
	colGray    = lipgloss.Color("8")
	colMuted   = lipgloss.Color("242")
	colWhite   = lipgloss.Color("15")
	colBlue    = lipgloss.Color("75")

	styleValid    = lipgloss.NewStyle().Foreground(colGreen).Bold(true)
	styleInvalid  = lipgloss.NewStyle().Foreground(colRed).Bold(true)
	styleExpired  = lipgloss.NewStyle().Foreground(colOrange).Bold(true)
	styleRateLim  = lipgloss.NewStyle().Foreground(colYellow).Bold(true)
	styleBlocked  = lipgloss.NewStyle().Foreground(colCyan).Bold(true)
	styleError    = lipgloss.NewStyle().Foreground(colRed).Bold(true)
	styleParseErr = lipgloss.NewStyle().Foreground(colRed).Bold(true)
	styleEmpty    = lipgloss.NewStyle().Foreground(colYellow).Bold(true)
	styleRetry    = lipgloss.NewStyle().Foreground(colYellow).Bold(true)
	stylePending  = lipgloss.NewStyle().Foreground(colGray)

	styleEmail       = lipgloss.NewStyle().Foreground(colCyan).Bold(true)
	stylePlanPro     = lipgloss.NewStyle().Foreground(colMagenta).Bold(true)
	stylePlanDefault = lipgloss.NewStyle().Foreground(colMuted)
	styleExpiry      = lipgloss.NewStyle().Foreground(colYellow)
	styleProxy       = lipgloss.NewStyle().Foreground(colBlue)
	styleElapsed     = lipgloss.NewStyle().Foreground(colGray)
	styleWarning     = lipgloss.NewStyle().Foreground(colYellow)
	styleFailDetail  = lipgloss.NewStyle().Foreground(lipgloss.Color("203"))
	styleMutedPath   = lipgloss.NewStyle().Foreground(colMuted)
	styleFileBase    = lipgloss.NewStyle().Foreground(colWhite)

	styleHeaderTitle = lipgloss.NewStyle().Bold(true).Foreground(colWhite)
	styleHeaderMeta  = lipgloss.NewStyle().Foreground(colMuted)
	styleColHeader   = lipgloss.NewStyle().Bold(true).Foreground(colMuted)
	styleDivider     = lipgloss.NewStyle().Foreground(lipgloss.Color("238"))
	styleSection     = lipgloss.NewStyle().Bold(true).Foreground(colGreen)
	styleBarFill     = lipgloss.NewStyle().Foreground(colGreen).Bold(true)
	styleBarEmpty    = lipgloss.NewStyle().Foreground(lipgloss.Color("238"))
)

// Model represents the streaming dual-waterfall Bubble Tea state.
type Model struct {
	endpoint   string
	mode       string
	nProxies   int
	workers    int
	totalFiles int64
	startTime  time.Time
	results    <-chan validator.Result

	recentAll    []validator.Result // top waterfall: all checked files
	recentValids []validator.Result // bottom waterfall: valid files only
	allValids    []validator.Result // full archive of all valids for exit output
	validFile    *os.File           // real-time append log

	spinnerIdx     int
	done           bool
	totalProcessed int64
	valid          int
	invalid        int
	expired        int
	rateLim        int
	blocked        int
	retries        int
	empty          int
	errCount       int
	headless       bool
}

// NewModel initializes the dual-waterfall TUI model.
func NewModel(endpoint, mode string, nProxies, workers int, totalFiles int64, results <-chan validator.Result) Model {
	vf, _ := os.OpenFile("valid_sessions.txt", os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0o644)

	return Model{
		endpoint:     endpoint,
		mode:         mode,
		nProxies:     nProxies,
		workers:      workers,
		totalFiles:   totalFiles,
		startTime:    time.Now(),
		results:      results,
		recentAll:    make([]validator.Result, 0, recentAllSize),
		recentValids: make([]validator.Result, 0, recentValidsSize),
		allValids:    make([]validator.Result, 0, 128),
		validFile:    vf,
		headless:     !isCharDevice(os.Stdout),
	}
}

func isCharDevice(f *os.File) bool {
	fi, err := f.Stat()
	if err != nil {
		return false
	}
	return (fi.Mode() & os.ModeCharDevice) != 0
}

func tickCmd() tea.Cmd {
	return tea.Tick(100*time.Millisecond, func(time.Time) tea.Msg { return tickMsg{} })
}

func waitForResult(ch <-chan validator.Result) tea.Cmd {
	return func() tea.Msg {
		res, ok := <-ch
		if !ok {
			return allDoneMsg{}
		}
		return resultMsg(res)
	}
}

func (m Model) Init() tea.Cmd {
	return tea.Batch(waitForResult(m.results), tickCmd())
}

func (m Model) Update(msg tea.Msg) (tea.Model, tea.Cmd) {
	switch msg := msg.(type) {
	case tea.KeyMsg:
		switch msg.String() {
		case "q", "ctrl+c":
			if m.validFile != nil {
				_ = m.validFile.Close()
			}
			return m, tea.Quit
		}

	case tickMsg:
		if !m.done {
			m.spinnerIdx = (m.spinnerIdx + 1) % len(spinnerFrames)
			return m, tickCmd()
		}

	case resultMsg:
		m.totalProcessed++
		res := validator.Result(msg)

		switch res.Status {
		case validator.StatusValid:
			m.valid++
			m.allValids = append(m.allValids, res)

			// Add to bottom waterfall
			m.recentValids = append(m.recentValids, res)
			if len(m.recentValids) > recentValidsSize {
				m.recentValids = m.recentValids[len(m.recentValids)-recentValidsSize:]
			}

			// Save to valid_sessions.txt in real-time
			if m.validFile != nil {
				line := fmt.Sprintf("%s\t%s\t%s\texp:%s\t%s\n", res.Path, res.Email, res.Plan, res.Expiry, res.Proxy)
				_, _ = m.validFile.WriteString(line)
			}

		case validator.StatusInvalid:
			m.invalid++
		case validator.StatusExpired:
			m.expired++
		case validator.StatusRateLimited:
			m.rateLim++
		case validator.StatusBlocked:
			m.blocked++
		case validator.StatusRetry:
			m.retries++
		case validator.StatusEmpty:
			m.empty++
		default:
			m.errCount++
		}

		// Add to top waterfall
		m.recentAll = append(m.recentAll, res)
		if len(m.recentAll) > recentAllSize {
			m.recentAll = m.recentAll[len(m.recentAll)-recentAllSize:]
		}

		if m.headless {
			fmt.Println(formatTableRow(res, "•"))
		}

		return m, waitForResult(m.results)

	case allDoneMsg:
		m.done = true
		if m.validFile != nil {
			_ = m.validFile.Close()
		}
		if m.headless {
			return m, tea.Quit
		}
		return m, nil
	}
	return m, nil
}

// truncateMiddle shortens s to maxLen, preserving head and tail with "..."
func truncateMiddle(s string, maxLen int) string {
	if len(s) <= maxLen {
		return s
	}
	if maxLen <= 5 {
		return s[:maxLen]
	}
	head := (maxLen - 3) / 2
	tail := maxLen - 3 - head
	return s[:head] + "..." + s[len(s)-tail:]
}

// padRight pads s with spaces to exactly width characters
func padRight(s string, width int) string {
	visLen := lipgloss.Width(s)
	if visLen == width {
		return s
	}
	if visLen > width {
		return s[:width]
	}
	return s + strings.Repeat(" ", width-visLen)
}

func formatStatus(s validator.Status, spinner string) string {
	switch s {
	case validator.StatusValid:
		return styleValid.Render("✔ VALID    ")
	case validator.StatusInvalid:
		return styleInvalid.Render("✖ INVALID  ")
	case validator.StatusExpired:
		return styleExpired.Render("⧖ EXPIRED  ")
	case validator.StatusRateLimited:
		return styleRateLim.Render("⏳ RATE_LIM ")
	case validator.StatusBlocked:
		return styleBlocked.Render("⛔ BLOCKED  ")
	case validator.StatusRetry:
		return styleRetry.Render("⚠ RETRY    ")
	case validator.StatusEmpty:
		return styleEmpty.Render("! EMPTY    ")
	case validator.StatusError:
		return styleError.Render("⚠ ERROR    ")
	case validator.StatusParseError:
		return styleParseErr.Render("! PARSE_ERR")
	case validator.StatusPending:
		return stylePending.Render(fmt.Sprintf("%s CHECKING ", spinner))
	default:
		return stylePending.Render("? UNKNOWN  ")
	}
}

// formatTableRow formats one row into strict fixed-width table columns.
// Total row width: 140 chars.
func formatTableRow(res validator.Result, spinner string) string {
	statusStr := formatStatus(res.Status, spinner)

	cleanPath := filepath.Clean(res.Path)
	fileStr := styleFileBase.Render(padRight(truncateMiddle(cleanPath, 34), 34))

	var detailStr string
	if res.Status == validator.StatusValid && res.Email != "" {
		detailStr = styleEmail.Render(padRight(truncateMiddle(res.Email, 32), 32))
	} else if res.Detail != "" {
		detailStr = styleFailDetail.Render(padRight(truncateMiddle(res.Detail, 32), 32))
	} else {
		detailStr = styleMutedPath.Render(padRight("-", 32))
	}

	var planStr string
	if res.Plan != "" {
		if strings.EqualFold(res.Plan, "Free") {
			planStr = stylePlanDefault.Render("[Free]  ")
		} else {
			planStr = stylePlanPro.Render(padRight("["+res.Plan+"]", 8))
		}
	} else {
		planStr = styleMutedPath.Render(padRight("-", 8))
	}

	var expStr string
	if res.Expiry != "" && res.Expiry != "Unknown" {
		expStr = styleExpiry.Render(padRight(res.Expiry, 17))
	} else {
		expStr = styleMutedPath.Render(padRight("-", 17))
	}

	var proxyStr string
	if res.Proxy != "" {
		proxyStr = styleProxy.Render(padRight(truncateMiddle(res.Proxy, 24), 24))
	} else {
		proxyStr = styleMutedPath.Render(padRight("direct", 24))
	}

	var timeStr string
	if res.Elapsed > 0 {
		timeStr = styleElapsed.Render(padRight(res.Elapsed.Round(time.Millisecond).String(), 7))
	} else {
		timeStr = styleElapsed.Render(padRight("-", 7))
	}

	return fmt.Sprintf(" %s  %s  %s  %s  %s  %s  %s",
		statusStr, fileStr, detailStr, planStr, expStr, proxyStr, timeStr)
}

func renderHeader(endpoint, mode string, nProxies, workers int) string {
	var b strings.Builder
	title := styleHeaderTitle.Render("◈ CHATGPT SESSION VALIDATOR")

	proxyStr := "direct"
	if nProxies > 0 {
		proxyStr = fmt.Sprintf("%s (%d proxies)", mode, nProxies)
	}
	meta := styleHeaderMeta.Render(fmt.Sprintf("endpoint: %s  •  workers: %d  •  proxy: %s", endpoint, workers, proxyStr))

	b.WriteString(title + "  " + meta + "\n")
	colHeaders := fmt.Sprintf(" %-11s  %-34s  %-32s  %-8s  %-17s  %-24s  %s",
		"STATUS", "COOKIE FILE", "ACCOUNT / DETAILS", "PLAN", "EXPIRY", "PROXY", "TIME")
	b.WriteString(styleColHeader.Render(colHeaders) + "\n")
	b.WriteString(styleDivider.Render(strings.Repeat("─", 140)) + "\n")
	return b.String()
}

func renderValidsHeader() string {
	var b strings.Builder
	colHeaders := fmt.Sprintf(" %-11s  %-34s  %-32s  %-8s  %-17s  %-24s  %s",
		"STATUS", "COOKIE FILE", "EMAIL", "PLAN", "EXPIRY", "PROXY", "TIME")
	b.WriteString(styleColHeader.Render(colHeaders) + "\n")
	return b.String()
}

func renderProgressBar(current, total int64, width int) string {
	if total <= 0 {
		return ""
	}
	ratio := float64(current) / float64(total)
	if ratio > 1.0 {
		ratio = 1.0
	}
	filled := int(ratio * float64(width))
	if filled > width {
		filled = width
	}
	empty := width - filled

	bar := styleBarFill.Render(strings.Repeat("█", filled)) + styleBarEmpty.Render(strings.Repeat("░", empty))
	return fmt.Sprintf("[%s]", bar)
}

func (m Model) summary() string {
	var parts []string
	if m.valid > 0 {
		parts = append(parts, styleValid.Render(fmt.Sprintf("✔ %d valid", m.valid)))
	}
	if m.invalid > 0 {
		parts = append(parts, styleInvalid.Render(fmt.Sprintf("✖ %d invalid", m.invalid)))
	}
	if m.expired > 0 {
		parts = append(parts, styleExpired.Render(fmt.Sprintf("⧖ %d expired", m.expired)))
	}
	if m.rateLim > 0 {
		parts = append(parts, styleRateLim.Render(fmt.Sprintf("⏳ %d rate limited", m.rateLim)))
	}
	if m.blocked > 0 {
		parts = append(parts, styleBlocked.Render(fmt.Sprintf("⛔ %d blocked", m.blocked)))
	}
	if m.retries > 0 {
		parts = append(parts, styleRetry.Render(fmt.Sprintf("⚠ %d retry", m.retries)))
	}
	if m.empty > 0 {
		parts = append(parts, styleEmpty.Render(fmt.Sprintf("! %d empty", m.empty)))
	}
	if m.errCount > 0 {
		parts = append(parts, styleError.Render(fmt.Sprintf("⚠ %d error", m.errCount)))
	}

	elapsed := time.Since(m.startTime)
	secs := elapsed.Seconds()
	speedStr := ""
	etaStr := ""
	pctStr := ""
	barStr := ""

	if m.totalFiles > 0 {
		pct := float64(m.totalProcessed) / float64(m.totalFiles) * 100
		pctStr = fmt.Sprintf("%.1f%%", pct)
		barStr = renderProgressBar(m.totalProcessed, m.totalFiles, 24)

		if secs > 1 && m.totalProcessed > 0 {
			speed := float64(m.totalProcessed) / secs
			speedStr = fmt.Sprintf("%.0f/s", speed)
			remaining := m.totalFiles - m.totalProcessed
			if remaining > 0 && speed > 0 {
				etaSecs := float64(remaining) / speed
				eta := time.Duration(etaSecs) * time.Second
				if eta > time.Hour {
					etaStr = fmt.Sprintf("ETA: %dh%dm", int(eta.Hours()), int(eta.Minutes())%60)
				} else if eta > time.Minute {
					etaStr = fmt.Sprintf("ETA: %dm%02ds", int(eta.Minutes()), int(eta.Seconds())%60)
				} else {
					etaStr = fmt.Sprintf("ETA: %02ds", int(eta.Seconds()))
				}
			}
		}
	}

	var statsLine string
	badge := lipgloss.NewStyle().Bold(true).Foreground(colWhite).Render(fmt.Sprintf("[%d / %d]", m.totalProcessed, m.totalFiles))
	if m.totalFiles <= 0 {
		badge = lipgloss.NewStyle().Bold(true).Foreground(colWhite).Render(fmt.Sprintf("[%d checked]", m.totalProcessed))
	}

	progressParts := []string{badge}
	if barStr != "" {
		progressParts = append(progressParts, barStr)
	}
	if pctStr != "" {
		progressParts = append(progressParts, lipgloss.NewStyle().Bold(true).Foreground(colCyan).Render(pctStr))
	}
	if speedStr != "" {
		progressParts = append(progressParts, lipgloss.NewStyle().Foreground(colMuted).Render("• "+speedStr))
	}
	if etaStr != "" {
		progressParts = append(progressParts, lipgloss.NewStyle().Foreground(colYellow).Render("• "+etaStr))
	}

	statsLine = strings.Join(progressParts, " ") + "\n " + strings.Join(parts, "   ")
	return statsLine
}

func (m Model) View() string {
	var b strings.Builder
	frame := spinnerFrames[m.spinnerIdx]

	// 1. Top Header
	b.WriteString("\n" + renderHeader(m.endpoint, m.mode, m.nProxies, m.workers))

	// 2. Section 1: Live Waterfall (All files stream, capped at recentAllSize)
	if len(m.recentAll) == 0 {
		b.WriteString(stylePending.Render(fmt.Sprintf(" %s Initializing workers and checking first files...", frame)) + "\n")
	} else {
		for _, res := range m.recentAll {
			b.WriteString(formatTableRow(res, frame) + "\n")
		}
	}

	// 3. Middle Divider & Progress Status
	b.WriteString(styleDivider.Render(strings.Repeat("─", 140)) + "\n")
	b.WriteString(" " + m.summary() + "\n")

	// 4. Section 2: Valids Waterfall (Shows only valid ones as they appear in real-time)
	b.WriteString("\n")
	validsTitle := styleSection.Render(fmt.Sprintf("◈ LIVE VALID SESSIONS (%d found)", len(m.allValids)))
	if len(m.allValids) > recentValidsSize {
		validsTitle += styleHeaderMeta.Render(fmt.Sprintf(" • showing recent %d", len(m.recentValids)))
	}
	b.WriteString(" " + validsTitle + "\n")
	b.WriteString(renderValidsHeader())

	if len(m.recentValids) == 0 {
		b.WriteString(" " + styleMutedPath.Render("No valid sessions yet...") + "\n")
	} else {
		for _, v := range m.recentValids {
			b.WriteString(formatTableRow(v, "✔") + "\n")
		}
	}

	// 5. Bottom Status
	if m.done {
		b.WriteString("\n " + styleValid.Render("✔ Scan Complete.") + " Press q to exit and print all valid accounts.\n")
	} else {
		b.WriteString("\n " + styleHeaderMeta.Render("Press q or ctrl+c to quit.") + "\n")
	}

	return b.String()
}

// AllValidsOutput generates the full complete list of all valid sessions
// printed directly to the terminal when the script finishes.
func (m Model) AllValidsOutput() string {
	var b strings.Builder
	if len(m.allValids) == 0 {
		b.WriteString("\n" + styleDivider.Render(strings.Repeat("─", 80)) + "\n")
		b.WriteString(" " + styleMutedPath.Render("No valid sessions found.") + "\n")
		b.WriteString(styleDivider.Render(strings.Repeat("─", 80)) + "\n")
		return b.String()
	}

	title := styleValid.Render(fmt.Sprintf("✔ ALL VALID SESSIONS (%d):", len(m.allValids)))
	b.WriteString("\n" + title + "\n")
	b.WriteString(styleDivider.Render(strings.Repeat("─", 140)) + "\n")
	b.WriteString(renderValidsHeader())

	for _, r := range m.allValids {
		b.WriteString(formatTableRow(r, "✔") + "\n")
	}
	b.WriteString(styleDivider.Render(strings.Repeat("─", 140)) + "\n")
	b.WriteString(fmt.Sprintf(" (Saved %d valid sessions to valid_sessions.txt)\n\n", len(m.allValids)))
	return b.String()
}

// Run launches the Bubble Tea program for the validation session.
func Run(endpoint, mode string, nProxies, workers int, totalFiles int64, results <-chan validator.Result) error {
	m := NewModel(endpoint, mode, nProxies, workers, totalFiles, results)
	var p *tea.Program
	if m.headless {
		p = tea.NewProgram(m, tea.WithInput(nil), tea.WithoutRenderer())
	} else {
		p = tea.NewProgram(m)
	}
	final, err := p.Run()
	if err != nil {
		return err
	}
	if fm, ok := final.(Model); ok {
		// When script finishes, print the complete list of ALL valid sessions
		fmt.Print(fm.AllValidsOutput())
	}
	return nil
}
