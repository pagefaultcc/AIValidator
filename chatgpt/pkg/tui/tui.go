package tui

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
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

const recentWindowSize = 15

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
)

// Model represents the streaming Bubble Tea state. Memory consumption is constant
// regardless of whether scanning 10 or 1,000,000 files.
type Model struct {
	endpoint        string
	mode            string
	nProxies        int
	workers         int
	totalDiscovered *atomic.Int64
	discoveryDone   *atomic.Bool
	results         <-chan validator.Result

	recent         []validator.Result // sliding window of recent items
	valids         []validator.Result // valid accounts collected for bottom display
	spinnerIdx     int
	done           bool
	totalProcessed int
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

// NewModel initializes the streaming TUI model.
func NewModel(endpoint, mode string, nProxies, workers int, totalDiscovered *atomic.Int64, discoveryDone *atomic.Bool, results <-chan validator.Result) Model {
	return Model{
		endpoint:        endpoint,
		mode:            mode,
		nProxies:        nProxies,
		workers:         workers,
		totalDiscovered: totalDiscovered,
		discoveryDone:   discoveryDone,
		results:         results,
		recent:          make([]validator.Result, 0, recentWindowSize),
		valids:          make([]validator.Result, 0, 64),
		headless:        !isCharDevice(os.Stdout),
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
			m.valids = append(m.valids, res)
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

		// Update sliding window
		m.recent = append(m.recent, res)
		if len(m.recent) > recentWindowSize {
			m.recent = m.recent[len(m.recent)-recentWindowSize:]
		}

		if m.headless {
			// In headless mode, output each result line in real-time
			fmt.Println(formatResultLine(res))
		}

		return m, waitForResult(m.results)

	case allDoneMsg:
		m.done = true
		if m.headless {
			return m, tea.Quit
		}
		return m, nil
	}
	return m, nil
}

func formatStatus(s validator.Status, spinner string) string {
	switch s {
	case validator.StatusValid:
		return styleValid.Render("✔ VALID     ")
	case validator.StatusInvalid:
		return styleInvalid.Render("✖ INVALID   ")
	case validator.StatusExpired:
		return styleExpired.Render("⧖ EXPIRED   ")
	case validator.StatusRateLimited:
		return styleRateLim.Render("⏳ RATE_LIM  ")
	case validator.StatusBlocked:
		return styleBlocked.Render("⛔ BLOCKED   ")
	case validator.StatusRetry:
		return styleRetry.Render("⚠ RETRY     ")
	case validator.StatusEmpty:
		return styleEmpty.Render("! EMPTY     ")
	case validator.StatusError:
		return styleError.Render("⚠ ERROR     ")
	case validator.StatusParseError:
		return styleParseErr.Render("! PARSE_ERR ")
	case validator.StatusPending:
		return stylePending.Render(fmt.Sprintf("%s CHECKING  ", spinner))
	default:
		return stylePending.Render("? UNKNOWN   ")
	}
}

func formatPath(p string, width int) string {
	clean := filepath.Clean(p)
	dir := filepath.Dir(clean)
	base := filepath.Base(clean)

	var formatted string
	if dir == "." || dir == "" {
		formatted = styleFileBase.Render(base)
	} else {
		formatted = styleMutedPath.Render(dir+string(filepath.Separator)) + styleFileBase.Render(base)
	}

	visLen := len(clean)
	if visLen < width {
		return formatted + strings.Repeat(" ", width-visLen)
	}
	return formatted
}

func formatDetail(res validator.Result) string {
	switch res.Status {
	case validator.StatusPending:
		return stylePending.Render("checking...")
	case validator.StatusValid:
		var parts []string
		if res.Email != "" {
			parts = append(parts, styleEmail.Render(res.Email))
		}
		if res.Plan != "" {
			if strings.EqualFold(res.Plan, "Free") {
				parts = append(parts, stylePlanDefault.Render("[Free]"))
			} else {
				parts = append(parts, stylePlanPro.Render("["+res.Plan+"]"))
			}
		}
		if res.Expiry != "" && res.Expiry != "Unknown" {
			parts = append(parts, styleExpiry.Render("exp: "+res.Expiry))
		}
		if res.Detail != "" {
			parts = append(parts, styleWarning.Render("("+res.Detail+")"))
		}
		return strings.Join(parts, "  ")
	case validator.StatusExpired:
		if res.Detail != "" {
			return styleExpired.Render(res.Detail)
		}
		return styleExpired.Render("session expired")
	case validator.StatusInvalid:
		if res.Detail != "" {
			return styleFailDetail.Render(res.Detail)
		}
		return styleFailDetail.Render("invalid authorization")
	case validator.StatusRateLimited:
		if res.Detail != "" {
			return styleWarning.Render(res.Detail)
		}
		return styleWarning.Render("rate limited")
	case validator.StatusBlocked:
		if res.Detail != "" {
			return styleBlocked.Render(res.Detail)
		}
		return styleBlocked.Render("cloudflare challenge")
	case validator.StatusRetry:
		if res.Detail != "" {
			return styleWarning.Render(res.Detail)
		}
		return styleWarning.Render("network error (retained)")
	case validator.StatusEmpty:
		return styleEmpty.Render("no cookies found")
	default:
		if res.Detail != "" {
			return styleFailDetail.Render(res.Detail)
		}
		return styleFailDetail.Render(res.Status.String())
	}
}

func formatProxy(proxy string) string {
	if proxy == "" {
		return ""
	}
	return styleProxy.Render("[" + proxy + "]")
}

func formatElapsed(d time.Duration) string {
	if d <= 0 {
		return ""
	}
	return styleElapsed.Render("(" + d.Round(time.Millisecond).String() + ")")
}

func formatResultLine(res validator.Result) string {
	statusStr := formatStatus(res.Status, "•")
	pathStr := formatPath(res.Path, 32)
	detailStr := formatDetail(res)
	proxyStr := formatProxy(res.Proxy)
	elapsedStr := formatElapsed(res.Elapsed)

	line := fmt.Sprintf(" %s  %s  %s", statusStr, pathStr, detailStr)
	if proxyStr != "" {
		line += "  " + proxyStr
	}
	if elapsedStr != "" {
		line += "  " + elapsedStr
	}
	return line
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
	colHeaders := fmt.Sprintf(" %-12s  %-32s  %s", "STATUS", "COOKIE FILE", "ACCOUNT DETAILS")
	b.WriteString(styleColHeader.Render(colHeaders) + "\n")
	b.WriteString(styleDivider.Render(strings.Repeat("─", 80)) + "\n")
	return b.String()
}

func (m Model) summary() string {
	var parts []string
	if m.valid > 0 {
		parts = append(parts, lipgloss.NewStyle().Foreground(colGreen).Bold(true).Render(fmt.Sprintf("✔ %d valid", m.valid)))
	}
	if m.invalid > 0 {
		parts = append(parts, lipgloss.NewStyle().Foreground(colRed).Bold(true).Render(fmt.Sprintf("✖ %d invalid", m.invalid)))
	}
	if m.expired > 0 {
		parts = append(parts, lipgloss.NewStyle().Foreground(colOrange).Bold(true).Render(fmt.Sprintf("⧖ %d expired", m.expired)))
	}
	if m.rateLim > 0 {
		parts = append(parts, lipgloss.NewStyle().Foreground(colYellow).Bold(true).Render(fmt.Sprintf("⏳ %d rate limited", m.rateLim)))
	}
	if m.blocked > 0 {
		parts = append(parts, lipgloss.NewStyle().Foreground(colCyan).Bold(true).Render(fmt.Sprintf("⛔ %d blocked", m.blocked)))
	}
	if m.retries > 0 {
		parts = append(parts, lipgloss.NewStyle().Foreground(colYellow).Bold(true).Render(fmt.Sprintf("⚠ %d retry", m.retries)))
	}
	if m.empty > 0 {
		parts = append(parts, lipgloss.NewStyle().Foreground(colYellow).Render(fmt.Sprintf("! %d empty", m.empty)))
	}
	if m.errCount > 0 {
		parts = append(parts, lipgloss.NewStyle().Foreground(colRed).Render(fmt.Sprintf("⚠ %d error", m.errCount)))
	}

	var badge string
	var totalDisc int64
	var isDone bool
	if m.totalDiscovered != nil {
		totalDisc = m.totalDiscovered.Load()
	}
	if m.discoveryDone != nil {
		isDone = m.discoveryDone.Load()
	}

	if isDone && totalDisc > 0 {
		badge = lipgloss.NewStyle().Bold(true).Foreground(colWhite).Render(fmt.Sprintf("[%d/%d]", m.totalProcessed, totalDisc))
	} else if totalDisc > 0 {
		badge = lipgloss.NewStyle().Bold(true).Foreground(colWhite).Render(fmt.Sprintf("[%d/%d...]", m.totalProcessed, totalDisc))
	} else {
		badge = lipgloss.NewStyle().Bold(true).Foreground(colWhite).Render(fmt.Sprintf("[%d checked]", m.totalProcessed))
	}

	if len(parts) == 0 {
		return badge + "  checking..."
	}
	return badge + "  " + strings.Join(parts, "   ")
}

func (m Model) View() string {
	var b strings.Builder
	b.WriteString("\n" + renderHeader(m.endpoint, m.mode, m.nProxies, m.workers))

	// Render rolling stream of recent files
	for _, res := range m.recent {
		b.WriteString(formatResultLine(res) + "\n")
	}

	b.WriteString(styleDivider.Render(strings.Repeat("─", 80)) + "\n")
	b.WriteString(" " + m.summary() + "\n")

	if m.done {
		b.WriteString(" " + lipgloss.NewStyle().Foreground(colGreen).Bold(true).Render("✔ Done.") + " Press q to exit.\n")
	} else {
		b.WriteString(" " + styleHeaderMeta.Render("Press q to quit.") + "\n")
	}

	// Valids printed cleanly at the very bottom, NOT between bars
	if len(m.valids) > 0 {
		b.WriteString(renderValidSummary(m.valids))
	} else if m.done {
		b.WriteString("\n " + lipgloss.NewStyle().Foreground(colMuted).Render("No valid sessions found.") + "\n")
	}

	return b.String()
}

// ValidSummary returns a formatted recap of all valid sessions at bottom.
func (m Model) ValidSummary() string {
	return renderValidSummary(m.valids)
}

func renderValidSummary(valids []validator.Result) string {
	var b strings.Builder
	if len(valids) == 0 {
		b.WriteString("\n " + lipgloss.NewStyle().Foreground(colMuted).Render("No valid sessions found.") + "\n")
		return b.String()
	}

	title := lipgloss.NewStyle().Bold(true).Foreground(colGreen).Render(fmt.Sprintf("✔ VALID SESSIONS (%d):", len(valids)))
	b.WriteString("\n" + title + "\n")

	for _, r := range valids {
		planBadge := stylePlanDefault.Render("[Free]")
		if r.Plan != "" {
			if strings.EqualFold(r.Plan, "Free") {
				planBadge = stylePlanDefault.Render("[Free]")
			} else {
				planBadge = stylePlanPro.Render("[" + r.Plan + "]")
			}
		}

		displayPath := r.MovedPath
		if displayPath == "" {
			displayPath = r.Path
		}
		pathStr := formatPath(displayPath, 32)
		var meta []string
		if r.Email != "" {
			meta = append(meta, styleEmail.Render(r.Email))
		}
		if r.Expiry != "" && r.Expiry != "Unknown" {
			meta = append(meta, styleExpiry.Render("exp: "+r.Expiry))
		}
		if r.Proxy != "" {
			meta = append(meta, styleProxy.Render("["+r.Proxy+"]"))
		}

		metaStr := strings.Join(meta, "  ")
		if metaStr != "" {
			b.WriteString(fmt.Sprintf("  ✔ %s  %-12s  %s\n", pathStr, planBadge, metaStr))
		} else {
			b.WriteString(fmt.Sprintf("  ✔ %s  %s\n", pathStr, planBadge))
		}
	}
	return b.String()
}

// Run launches the Bubble Tea program for the validation session.
func Run(endpoint, mode string, nProxies, workers int, totalDiscovered *atomic.Int64, discoveryDone *atomic.Bool, results <-chan validator.Result) error {
	m := NewModel(endpoint, mode, nProxies, workers, totalDiscovered, discoveryDone, results)
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
		if !m.headless {
			// Print valids recap at the bottom of the terminal output
			fmt.Print(fm.ValidSummary() + "\n")
		} else {
			fmt.Println("\n" + fm.summary())
			fmt.Print(fm.ValidSummary() + "\n")
		}
	}
	return nil
}
