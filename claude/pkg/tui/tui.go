package tui

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"

	tea "github.com/charmbracelet/bubbletea"
	"github.com/charmbracelet/lipgloss"

	"claude-validator/pkg/cookie"
	"claude-validator/pkg/validator"
)

type (
	resultMsg  validator.Result
	allDoneMsg struct{}
	tickMsg    struct{}
)

// ScanFunc runs one full scan (discover, parse, worker pump) for a folder and
// returns the discovered rows plus the streaming results channel.
type ScanFunc func(dir, pattern string) ([]string, []*cookie.CookieFile, <-chan validator.Result, error)

// scanStartedMsg carries a finished scan: rows ready, workers running.
type scanStartedMsg struct {
	dir     string
	paths   []string
	parsed  []*cookie.CookieFile
	results <-chan validator.Result
}

type scanErrorMsg struct{ err error }

var spinnerFrames = []string{"⠋", "⠙", "⠹", "⠸", "⠼", "⠴", "⠦", "⠧", "⠇", "⠏"}

// maxShownRows caps the interactive list: above this size the TUI switches to a
// progress line plus a rolling window of recent results.
const maxShownRows = 40

// recentRows is the rolling window size for large runs.
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
	stylePending  = lipgloss.NewStyle().Foreground(colGray)

	styleEmail       = lipgloss.NewStyle().Foreground(colCyan).Bold(true)
	stylePlanPro     = lipgloss.NewStyle().Foreground(colMagenta).Bold(true)
	stylePlanDefault = lipgloss.NewStyle().Foreground(colMuted)
	styleOrgsLabel   = lipgloss.NewStyle().Foreground(colMuted)
	styleOrgsVal     = lipgloss.NewStyle().Foreground(colWhite)
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

type Model struct {
	paths    []string
	rows     []row
	byPath   map[string]int // path -> index in rows for O(1) updates
	recent   []int          // indices of recent completions, capped at recentRows
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
	errCount   int
	headless   bool

	// Interactive mode (-tui): folder prompt state.
	scan    ScanFunc
	prompt  bool
	input   string
	hint    string
	lastDir string
}

func NewModel(paths []string, parsed []*cookie.CookieFile, endpoint, mode string, nProxies, workers int, results <-chan validator.Result) Model {
	m := Model{
		endpoint: endpoint,
		mode:     mode,
		nProxies: nProxies,
		workers:  workers,
		results:  results,
		headless: !isCharDevice(os.Stdout),
	}
	m.resetRun(paths, parsed, results)
	return m
}

// NewInteractiveModel starts in prompt state; -tui users pick the folder there.
func NewInteractiveModel(scan ScanFunc, endpoint, mode string, nProxies, workers int) Model {
	return Model{
		scan:     scan,
		prompt:   true,
		input:    "cookies",
		lastDir:  "cookies",
		endpoint: endpoint,
		mode:     mode,
		nProxies: nProxies,
		workers:  workers,
	}
}

// resetRun (re)initializes all per-run state for a freshly discovered set.
func (m *Model) resetRun(paths []string, parsed []*cookie.CookieFile, results <-chan validator.Result) {
	rows := make([]row, len(paths))
	byPath := make(map[string]int, len(paths))
	for i, p := range paths {
		rows[i] = row{path: p, cf: parsed[i]}
		byPath[p] = i
	}
	m.paths = paths
	m.rows = rows
	m.byPath = byPath
	m.recent = nil
	if results != nil {
		m.results = results
	}
	m.spinnerIdx = 0
	m.done = false
	m.valid, m.invalid, m.expired, m.rateLim, m.blocked, m.errCount = 0, 0, 0, 0, 0, 0
}

// toPrompt returns to the folder-choosing state after a finished run.
func (m *Model) toPrompt() {
	m.prompt = true
	m.input = m.lastDir
	m.hint = ""
	m.paths, m.rows, m.byPath, m.recent, m.results = nil, nil, nil, nil, nil
	m.done = false
	m.valid, m.invalid, m.expired, m.rateLim, m.blocked, m.errCount = 0, 0, 0, 0, 0, 0
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

// waitForResult is the one pending channel receive; every resultMsg re-arms it.
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
	if m.prompt {
		return nil
	}
	return tea.Batch(waitForResult(m.results), tickCmd())
}

func (m Model) Update(msg tea.Msg) (tea.Model, tea.Cmd) {
	switch msg := msg.(type) {
	case tea.KeyMsg:
		if m.prompt {
			return m.updatePrompt(msg)
		}
		switch msg.String() {
		case "q", "ctrl+c":
			return m, tea.Quit
		case "n":
			if m.done {
				m.toPrompt()
				return m, nil
			}
		}

	case tickMsg:
		if m.prompt {
			return m, nil // no spinner in prompt state; stop ticking
		}
		if !m.done {
			m.spinnerIdx = (m.spinnerIdx + 1) % len(spinnerFrames)
			return m, tickCmd()
		}

	case scanStartedMsg:
		m.lastDir = msg.dir
		if len(msg.paths) == 0 {
			m.hint = fmt.Sprintf("no files match under %s", msg.dir)
			return m, nil
		}
		m.hint = ""
		m.resetRun(msg.paths, msg.parsed, msg.results)
		m.prompt = false
		return m, tea.Batch(waitForResult(m.results), tickCmd())

	case scanErrorMsg:
		m.hint = "error: " + msg.err.Error()
		return m, nil
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
			if strings.EqualFold(res.Plan, "default_claude_ai") {
				parts = append(parts, stylePlanDefault.Render("[free]"))
			} else {
				parts = append(parts, stylePlanPro.Render("["+res.Plan+"]"))
			}
		}
		if res.Orgs != "" {
			parts = append(parts, styleOrgsLabel.Render("orgs:")+" "+styleOrgsVal.Render(res.Orgs))
		}
		if res.Detail != "" {
			parts = append(parts, styleWarning.Render("("+res.Detail+")"))
		}
		return strings.Join(parts, "  ")
	case validator.StatusExpired:
		if res.Detail != "" {
			return styleExpired.Render(res.Detail)
		}
		return styleExpired.Render("sessionKey expired")
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

func (r row) line(frame, mode string) string {
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
	title := styleHeaderTitle.Render("◈ CLAUDE SESSION VALIDATOR")

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
	if m.errCount > 0 {
		parts = append(parts, lipgloss.NewStyle().Foreground(colRed).Render(fmt.Sprintf("⚠ %d error", m.errCount)))
	}
	total := len(m.rows)
	done := m.valid + m.invalid + m.expired + m.rateLim + m.blocked + m.errCount

	badge := lipgloss.NewStyle().Bold(true).Foreground(colWhite).Render(fmt.Sprintf("[%d/%d]", done, total))
	if len(parts) == 0 {
		return badge + "  checking..."
	}
	return badge + "  " + strings.Join(parts, "   ")
}

func (m Model) View() string {
	var b strings.Builder
	if m.prompt {
		b.WriteString("\n" + renderHeader(m.endpoint, m.mode, m.nProxies, m.workers))
		b.WriteString("\n  folder with cookie .txt files:\n\n    " + m.input + "▌\n\n")
		b.WriteString("  enter scan · esc quit\n")
		if m.hint != "" {
			b.WriteString("\n  " + styleWarning.Render(m.hint) + "\n")
		}
		return b.String()
	}
	frame := spinnerFrames[m.spinnerIdx]
	b.WriteString("\n" + renderHeader(m.endpoint, m.mode, m.nProxies, m.workers))

	if len(m.rows) <= maxShownRows {
		for _, r := range m.rows {
			b.WriteString(r.line(frame, m.mode))
			b.WriteString("\n")
		}
	} else {
		// Large run: render a progress line plus the rolling window of recent
		// completions so the TUI stays snappy with thousands of files.
		for _, idx := range m.recent {
			b.WriteString(m.rows[idx].line(frame, m.mode))
			b.WriteString("\n")
		}
	}

	b.WriteString(styleDivider.Render(strings.Repeat("─", 80)) + "\n")
	b.WriteString(" " + m.summary() + "\n")
	if m.done {
		b.WriteString(" " + lipgloss.NewStyle().Foreground(colGreen).Bold(true).Render("✔ Done.") + " Press q to exit.\n")
		b.WriteString(m.ValidSummary())
	} else {
		b.WriteString("\n " + styleHeaderMeta.Render("Press q to quit.") + "\n")
	}
	return b.String()
}

// ValidSummary returns a formatted list of all valid sessions with their plans.
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
		b.WriteString("\n " + lipgloss.NewStyle().Foreground(colMuted).Render("No valid sessions found.") + "\n")
		return b.String()
	}

	title := lipgloss.NewStyle().Bold(true).Foreground(colGreen).Render(fmt.Sprintf("✔ VALID SESSIONS (%d):", len(rows)))
	b.WriteString("\n" + title + "\n")

	for _, r := range rows {
		planBadge := stylePlanDefault.Render("[free]")
		if r.res.Plan != "" {
			if strings.EqualFold(r.res.Plan, "default_claude_ai") {
				planBadge = stylePlanDefault.Render("[free]")
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
		if r.res.Orgs != "" {
			meta = append(meta, styleOrgsLabel.Render("orgs:")+" "+styleOrgsVal.Render(r.res.Orgs))
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
	return b.String()
}

// PlainOutput is the headless (piped) output: one line per file plus the
// summary and valid accounts recap, complete regardless of run size.
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

// Run executes the Bubble Tea program for the given validation run.
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
			// Print valid sessions recap directly to terminal so it persists after TUI exit
			fmt.Print("\n" + fm.ValidSummary() + "\n")
		}
	}
	return nil
}

// RunInteractive executes the Bubble Tea program in prompt-first mode: the
// user types a folder path, enter scans it, n returns to the prompt.
func RunInteractive(m Model) error {
	_, err := tea.NewProgram(m).Run()
	return err
}

// scanCmd runs one scan; discovery and parsing are local and fast, the worker
// pump keeps running in background goroutines.
func scanCmd(scan ScanFunc, dir string) tea.Cmd {
	return func() tea.Msg {
		paths, parsed, results, err := scan(dir, "*.txt")
		if err != nil {
			return scanErrorMsg{err}
		}
		return scanStartedMsg{dir: dir, paths: paths, parsed: parsed, results: results}
	}
}

// expandHome turns a leading ~ into the user's home directory.
func expandHome(p string) string {
	if p == "~" || strings.HasPrefix(p, "~/") {
		if home, err := os.UserHomeDir(); err == nil {
			return filepath.Join(home, strings.TrimPrefix(p, "~"))
		}
	}
	return p
}

// updatePrompt handles the minimal folder-path input: runes, backspace, clear,
// space, enter to scan, esc/ctrl+c to quit.
func (m Model) updatePrompt(msg tea.KeyMsg) (tea.Model, tea.Cmd) {
	switch msg.Type {
	case tea.KeyEnter:
		dir := expandHome(strings.TrimSpace(m.input))
		if dir == "" {
			m.hint = "type a folder path first"
			return m, nil
		}
		m.hint = ""
		return m, scanCmd(m.scan, dir)
	case tea.KeyBackspace:
		r := []rune(m.input)
		if len(r) > 0 {
			m.input = string(r[:len(r)-1])
		}
	case tea.KeyCtrlU:
		m.input = ""
	case tea.KeySpace:
		m.input += " "
	case tea.KeyEsc, tea.KeyCtrlC:
		return m, tea.Quit
	case tea.KeyRunes:
		m.input += string(msg.Runes)
	}
	return m, nil
}
