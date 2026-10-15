package main

import (
	"fmt"
	"os"
	"strings"
	"time"

	tea "github.com/charmbracelet/bubbletea"
	"github.com/charmbracelet/lipgloss"
)

type (
	resultMsg  Result
	allDoneMsg struct{}
	tickMsg    struct{}
)

var spinnerFrames = []string{"⠋", "⠙", "⠹", "⠸", "⠼", "⠴", "⠦", "⠧", "⠇", "⠏"}

// maxShownRows caps the interactive list: above this size the TUI switches to a
// progress line plus a rolling window of recent results. Rendering thousands of
// rows ten times per second is wasted work the terminal cannot display anyway.
const maxShownRows = 40

// recentRows is the rolling window size for large runs.
const recentRows = 15

type row struct {
	path string
	res  *Result // nil while the check is still running
}

type model struct {
	rows     []row
	byPath   map[string]int // O(1) row updates: no linear scan per result
	recent   []row          // rolling window of most recent results (large runs)
	results  <-chan Result
	frame    int
	header   string
	mode     string
	nProxies int
	width    int
	allDone  bool
	headless bool // non-TTY stdout: auto-quit when done, full plain output after Run
	done     int
	nValid   int
	nInvalid int
	nErrs    int
}

func newModel(paths []string, parsed []*CookieFile, endpoint, mode string, nProxies, workers int, results <-chan Result) model {
	rows := make([]row, len(paths))
	byPath := make(map[string]int, len(paths))
	done, errs := 0, 0
	for i, p := range paths {
		rows[i] = row{path: p}
		byPath[p] = i
		if parsed[i].Err != nil {
			rows[i].res = &Result{Path: p, Status: StatusParseError, Detail: parsed[i].Err.Error()}
			done++
			errs++
		}
	}
	proxyWord := "proxies"
	if nProxies == 1 {
		proxyWord = "proxy"
	}
	return model{
		rows:     rows,
		byPath:   byPath,
		results:  results,
		mode:     mode,
		nProxies: nProxies,
		done:     done,
		nErrs:    errs,
		header:   fmt.Sprintf("%s · mode: %s · %d %s · %d workers", endpoint, mode, nProxies, proxyWord, workers),
		headless: !isCharDevice(os.Stdout),
	}
}

func isCharDevice(f *os.File) bool {
	fi, err := f.Stat()
	if err != nil {
		return false
	}
	return fi.Mode()&os.ModeCharDevice != 0
}

func tickCmd() tea.Cmd {
	return tea.Tick(100*time.Millisecond, func(time.Time) tea.Msg { return tickMsg{} })
}

// waitForResult is the one pending channel receive; every resultMsg re-arms it.
func waitForResult(ch <-chan Result) tea.Cmd {
	return func() tea.Msg {
		r, ok := <-ch
		if !ok {
			return allDoneMsg{}
		}
		return resultMsg(r)
	}
}

func (m model) Init() tea.Cmd {
	return tea.Batch(waitForResult(m.results), tickCmd())
}

func (m model) Update(msg tea.Msg) (tea.Model, tea.Cmd) {
	switch msg := msg.(type) {
	case tickMsg:
		m.frame++
		return m, tickCmd()
	case tea.WindowSizeMsg:
		m.width = msg.Width
		return m, nil
	case resultMsg:
		r := Result(msg)
		i, ok := m.byPath[r.Path]
		if ok {
			cp := r
			m.rows[i].res = &cp
			if len(m.rows) > maxShownRows {
				m.recent = append(m.recent, m.rows[i])
				if len(m.recent) > recentRows {
					m.recent = m.recent[len(m.recent)-recentRows:]
				}
			}
		}
		m.done++
		switch r.Status {
		case StatusValid:
			m.nValid++
		case StatusInvalid, StatusExpired, StatusRateLimited, StatusBlocked:
			m.nInvalid++
		default:
			m.nErrs++
		}
		if m.done >= len(m.rows) {
			m.allDone = true
			if m.headless {
				return m, tea.Quit
			}
			return m, nil
		}
		return m, waitForResult(m.results)
	case allDoneMsg:
		m.allDone = true
		if m.headless {
			return m, tea.Quit
		}
		return m, nil
	case tea.KeyMsg:
		switch msg.String() {
		case "q", "ctrl+c":
			return m, tea.Quit
		}
	}
	return m, nil
}

func statusStyle(s Status) lipgloss.Style {
	base := lipgloss.NewStyle()
	switch s {
	case StatusValid:
		return base.Foreground(lipgloss.Color("2"))
	case StatusInvalid, StatusExpired, StatusParseError:
		return base.Foreground(lipgloss.Color("1"))
	case StatusRateLimited:
		return base.Foreground(lipgloss.Color("3"))
	case StatusBlocked:
		return base.Foreground(lipgloss.Color("6"))
	case StatusError:
		return base.Foreground(lipgloss.Color("8"))
	default:
		return base
	}
}

func (r row) detail(mode string) string {
	res := r.res
	if res == nil {
		return ""
	}
	var parts []string
	if res.Detail != "" {
		parts = append(parts, res.Detail)
	}
	if res.Status == StatusValid {
		for _, v := range []string{res.Email, res.Plan, res.Orgs} {
			if v != "" {
				parts = append(parts, v)
			}
		}
	}
	if res.Proxy != "" {
		parts = append(parts, res.Proxy+" ("+mode+")")
	}
	parts = append(parts, res.Elapsed.Round(time.Millisecond).String())
	return strings.Join(parts, " · ")
}

func (r row) line(frame, mode string) string {
	if r.res == nil {
		return fmt.Sprintf("%s %-12s %s", frame, "CHECKING", r.path)
	}
	status := statusStyle(r.res.Status).Render(fmt.Sprintf("%-12s", r.res.Status.String()))
	line := status + " " + r.path
	if d := r.detail(mode); d != "" {
		line += "  " + d
	}
	return line
}

func (m model) summary() string {
	return fmt.Sprintf("%d checked · %d valid · %d invalid · %d errors · %d proxies",
		m.done, m.nValid, m.nInvalid, m.nErrs, m.nProxies)
}

func (m model) View() string {
	var b strings.Builder
	b.WriteString(lipgloss.NewStyle().Bold(true).Render(m.header) + "\n")
	frame := spinnerFrames[m.frame%len(spinnerFrames)]

	if len(m.rows) <= maxShownRows {
		// Small runs: the original full-list view.
		for _, r := range m.rows {
			b.WriteString(r.line(frame, m.mode) + "\n")
		}
	} else {
		// Large runs: progress line + rolling window of recent results.
		if !m.allDone {
			b.WriteString(fmt.Sprintf("%s %d/%d checked · %d valid · %d invalid · %d errors\n",
				frame, m.done, len(m.rows), m.nValid, m.nInvalid, m.nErrs))
			b.WriteString(fmt.Sprintf("… %d rows not shown; full list is printed on exit\n", len(m.rows)-m.done))
		}
		for _, r := range m.recent {
			b.WriteString(r.line("", m.mode) + "\n")
		}
	}

	if m.allDone {
		b.WriteString("\n" + lipgloss.NewStyle().Bold(true).Render(m.summary()) + "\n")
		if !m.headless {
			b.WriteString("press q to quit (full list was printed if piped)\n")
		}
	}
	return b.String()
}

// plainOutput is the headless (piped) output: one line per file plus the
// summary, complete regardless of run size.
func (m model) plainOutput() string {
	var b strings.Builder
	b.WriteString(m.header + "\n")
	for _, r := range m.rows {
		b.WriteString(r.line("", m.mode) + "\n")
	}
	b.WriteString(m.summary() + "\n")
	return b.String()
}
