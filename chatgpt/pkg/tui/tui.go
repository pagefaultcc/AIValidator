package tui

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"

	tea "github.com/charmbracelet/bubbletea"
	"github.com/charmbracelet/lipgloss"

	"chatgpt-validator/pkg/cookie"
	"chatgpt-validator/pkg/validator"
)

type (
	resultMsg  validator.Result
	allDoneMsg struct{}
	tickMsg    struct{}
)

var spinnerFrames = []string{"⠋", "⠙", "⠹", "⠸", "⠼", "⠴", "⠦", "⠧", "⠇", "⠏"}

const maxShownRows = 40
const recentRows = 15

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

type row struct {
	path string
	cf   *cookie.CookieFile
	res  validator.Result
}

// Model represents the Bubble Tea state.
type Model struct {
	paths    []string
	rows     []row
	byPath   map[string]int
	recent   []int
	endpoint string
	mode     string
	nProxies int
	workers  int
	results  <-chan validator.Result

	spinnerIdx int
	done       bool
	valid      int
	invalid    int
	expired    int
	rateLim    int
	blocked    int
	retries    int
	empty      int
	errCount   int
	headless   bool
}

// NewModel initializes the TUI model.
func NewModel(paths []string, parsed []*cookie.CookieFile, endpoint, mode string, nProxies, workers int, results <-chan validator.Result) Model {
	rows := make([]row, len(paths))
	byPath := make(map[string]int, len(paths))
	for i, p := range paths {
		rows[i] = row{path: p, cf: parsed[i]}
		byPath[p] = i
	}
	return Model{
		paths:    paths,
		rows:     rows,
		byPath:   byPath,
		endpoint: endpoint,
		mode:     mode,
		nProxies: nProxies,
		workers:  workers,
		results:  results,
		headless: !isCharDevice(os.Stdout),
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
		if idx, ok := m.byPath[msg.Path]; ok {
			m.rows[idx].res = validator.Result(msg)
			switch msg.Status {
			case validator.StatusValid:
				m.valid++
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
			m.recent = append(m.recent, idx)
			if len(m.recent) > recentRows {
				m.recent = m.recent[len(m.recent)-recentRows:]
			}
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

func (r row) line(frame string) string {
	statusStr := formatStatus(r.res.Status, frame)
	pathStr := formatPath(r.path, 32)
	detailStr := formatDetail(r.res)
	proxyStr := formatProxy(r.res.Proxy)
	elapsedStr := formatElapsed(r.res.Elapsed)

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
	total := len(m.rows)
	done := m.valid + m.invalid + m.expired + m.rateLim + m.blocked + m.retries + m.empty + m.errCount

	badge := lipgloss.NewStyle().Bold(true).Foreground(colWhite).Render(fmt.Sprintf("[%d/%d]", done, total))
	if len(parts) == 0 {
		return badge + "  checking..."
	}
	return badge + "  " + strings.Join(parts, "   ")
}

func (m Model) View() string {
	var b strings.Builder
	frame := spinnerFrames[m.spinnerIdx]
	b.WriteString("\n" + renderHeader(m.endpoint, m.mode, m.nProxies, m.workers))

	if len(m.rows) <= maxShownRows {
		for _, r := range m.rows {
			b.WriteString(r.line(frame))
			b.WriteString("\n")
		}
	} else {
		for _, idx := range m.recent {
			b.WriteString(m.rows[idx].line(frame))
			b.WriteString("\n")
		}
	}

	b.WriteString(styleDivider.Render(strings.Repeat("─", 80)) + "\n")
	b.WriteString(" " + m.summary() + "\n")
	if m.done {
		b.WriteString("\n" + m.ValidSummary())
		b.WriteString(" " + lipgloss.NewStyle().Foreground(colGreen).Bold(true).Render("✔ Done.") + " Press q to exit.\n")
	} else {
		b.WriteString("\n " + styleHeaderMeta.Render("Press q to quit.") + "\n")
	}
	return b.String()
}

// ValidSummary returns a formatted recap of all valid sessions.
func (m Model) ValidSummary() string {
	var valids []row
	for _, r := range m.rows {
		if r.res.Status == validator.StatusValid {
			valids = append(valids, r)
		}
	}
	return renderValidSummary(valids)
}

func renderValidSummary(rows []row) string {
	var b strings.Builder
	if len(rows) == 0 {
		b.WriteString(styleDivider.Render(strings.Repeat("─", 80)) + "\n")
		b.WriteString(" " + lipgloss.NewStyle().Foreground(colMuted).Render("No valid sessions found.") + "\n")
		b.WriteString(styleDivider.Render(strings.Repeat("─", 80)) + "\n")
		return b.String()
	}

	title := lipgloss.NewStyle().Bold(true).Foreground(colGreen).Render(fmt.Sprintf("✔ VALID SESSIONS (%d):", len(rows)))
	b.WriteString(title + "\n")
	b.WriteString(styleDivider.Render(strings.Repeat("─", 80)) + "\n")

	for _, r := range rows {
		planBadge := stylePlanDefault.Render("[Free]")
		if r.res.Plan != "" {
			if strings.EqualFold(r.res.Plan, "Free") {
				planBadge = stylePlanDefault.Render("[Free]")
			} else {
				planBadge = stylePlanPro.Render("[" + r.res.Plan + "]")
			}
		}

		displayPath := r.res.MovedPath
		if displayPath == "" {
			displayPath = r.path
		}
		pathStr := formatPath(displayPath, 32)
		var meta []string
		if r.res.Email != "" {
			meta = append(meta, styleEmail.Render(r.res.Email))
		}
		if r.res.Expiry != "" && r.res.Expiry != "Unknown" {
			meta = append(meta, styleExpiry.Render("exp: "+r.res.Expiry))
		}
		if r.res.Proxy != "" {
			meta = append(meta, styleProxy.Render("["+r.res.Proxy+"]"))
		}

		metaStr := strings.Join(meta, "  ")
		if metaStr != "" {
			b.WriteString(fmt.Sprintf("  ✔ %s  %-12s  %s\n", pathStr, planBadge, metaStr))
		} else {
			b.WriteString(fmt.Sprintf("  ✔ %s  %s\n", pathStr, planBadge))
		}
	}
	b.WriteString(styleDivider.Render(strings.Repeat("─", 80)) + "\n")
	return b.String()
}

// PlainOutput outputs complete non-interactive lines for pipes and redirects.
func (m Model) PlainOutput() string {
	var b strings.Builder
	b.WriteString("\n" + renderHeader(m.endpoint, m.mode, m.nProxies, m.workers))
	for _, r := range m.rows {
		statusStr := formatStatus(r.res.Status, "•")
		pathStr := formatPath(r.path, 32)
		detailStr := formatDetail(r.res)
		proxyStr := formatProxy(r.res.Proxy)
		elapsedStr := formatElapsed(r.res.Elapsed)

		line := fmt.Sprintf(" %s  %s  %s", statusStr, pathStr, detailStr)
		if proxyStr != "" {
			line += "  " + proxyStr
		}
		if elapsedStr != "" {
			line += "  " + elapsedStr
		}
		b.WriteString(line + "\n")
	}
	b.WriteString(styleDivider.Render(strings.Repeat("─", 80)) + "\n")
	b.WriteString(" " + m.summary() + "\n\n")
	b.WriteString(m.ValidSummary() + "\n")
	return b.String()
}

// Run launches the Bubble Tea program for the validation session.
func Run(paths []string, parsed []*cookie.CookieFile, endpoint, mode string, nProxies, workers int, results <-chan validator.Result) error {
	m := NewModel(paths, parsed, endpoint, mode, nProxies, workers, results)
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
		if m.headless {
			fmt.Print(fm.PlainOutput())
		} else {
			fmt.Print("\n" + fm.ValidSummary() + "\n")
		}
	}
	return nil
}
