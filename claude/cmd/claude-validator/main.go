package main

import (
	"context"
	"flag"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"claude-validator/pkg/cookie"
	"claude-validator/pkg/proxy"
	"claude-validator/pkg/tui"
	"claude-validator/pkg/validator"

	"github.com/charmbracelet/lipgloss"
)

func renderColoredHelp() string {
	var b strings.Builder

	titleStyle := lipgloss.NewStyle().Bold(true).Foreground(lipgloss.Color("51"))
	badgeStyle := lipgloss.NewStyle().Bold(true).Foreground(lipgloss.Color("82"))
	subStyle := lipgloss.NewStyle().Foreground(lipgloss.Color("246"))
	sectionStyle := lipgloss.NewStyle().Bold(true).Foreground(lipgloss.Color("220"))
	flagStyle := lipgloss.NewStyle().Bold(true).Foreground(lipgloss.Color("15"))
	aliasStyle := lipgloss.NewStyle().Foreground(lipgloss.Color("75"))
	typeStyle := lipgloss.NewStyle().Foreground(lipgloss.Color("213"))
	docStyle := lipgloss.NewStyle().Foreground(lipgloss.Color("252"))
	defaultStyle := lipgloss.NewStyle().Foreground(lipgloss.Color("244"))
	dividerStyle := lipgloss.NewStyle().Foreground(lipgloss.Color("238"))

	b.WriteString("\n")
	b.WriteString(titleStyle.Render("◈ CLAUDE SESSION & COOKIE VALIDATOR ") + badgeStyle.Render("[GO EDITION]") + "\n")
	b.WriteString(subStyle.Render("  High-speed multi-threaded session validator for Claude.ai") + "\n\n")

	b.WriteString(sectionStyle.Render("USAGE:") + "\n")
	b.WriteString("  " + flagStyle.Render("claude.exe") + " " + typeStyle.Render("[options...]") + "\n\n")

	b.WriteString(dividerStyle.Render(strings.Repeat("─", 80)) + "\n")
	b.WriteString(sectionStyle.Render("FLAGS & OPTIONS:") + "\n\n")

	type opt struct {
		flags string
		val   string
		desc  string
		def   string
	}

	sections := []struct {
		name string
		opts []opt
	}{
		{
			name: "TARGET DIRECTORY / FILES",
			opts: []opt{
				{
					flags: "-d, -dir, -f, -folder",
					val:   "<path>",
					desc:  "Cookie file or folder to scan recursively",
					def:   "cookies/",
				},
				{
					flags: "-pattern",
					val:   "<glob>",
					desc:  "Base-name glob when target is a folder",
					def:   "*",
				},
				{
					flags: "-tui",
					val:   "",
					desc:  "Interactive mode: pick cookie folder inside the TUI",
					def:   "false",
				},
			},
		},
		{
			name: "CONCURRENCY & TIMINGS",
			opts: []opt{
				{
					flags: "-w, -workers",
					val:   "<int>",
					desc:  "Concurrent checks (min 1)",
					def:   "5",
				},
				{
					flags: "-t, -timeout",
					val:   "<duration>",
					desc:  "Per-request network timeout",
					def:   "30s",
				},
				{
					flags: "-e, -endpoint",
					val:   "<url>",
					desc:  "Claude API base URL",
					def:   "https://claude.ai",
				},
			},
		},
		{
			name: "PROXY CONFIGURATION",
			opts: []opt{
				{
					flags: "-p, -proxy, -proxies",
					val:   "<file>",
					desc:  "Proxy list file (used only if it exists)",
					def:   "proxies.txt",
				},
				{
					flags: "-m, -mode, -proxy-mode",
					val:   "<mode>",
					desc:  "sticky (pin file to 1 proxy) or rotating (round-robin)",
					def:   "sticky",
				},
			},
		},
		{
			name: "OUTPUT DIRECTORIES",
			opts: []opt{
				{
					flags: "-v, -valid, -valid-dir",
					val:   "<folder>",
					desc:  "Folder to move valid cookie files to (empty disables)",
					def:   "valid",
				},
				{
					flags: "-i, -invalid, -invalid-dir",
					val:   "<folder>",
					desc:  "Folder to move invalid cookie files to (empty disables)",
					def:   "invalid",
				},
			},
		},
		{
			name: "HELP & INFO",
			opts: []opt{
				{
					flags: "-h, --help",
					val:   "",
					desc:  "Show this high-visibility colored help screen and exit",
					def:   "",
				},
			},
		},
	}

	for _, sec := range sections {
		b.WriteString(aliasStyle.Render("• "+sec.name) + "\n")
		for _, o := range sec.opts {
			flagPart := "  " + flagStyle.Render(o.flags)
			if o.val != "" {
				flagPart += " " + typeStyle.Render(o.val)
			}
			visLen := lipgloss.Width(flagPart)
			padding := 36 - visLen
			if padding < 2 {
				padding = 2
			}
			line := flagPart + strings.Repeat(" ", padding) + docStyle.Render(o.desc)
			if o.def != "" {
				line += " " + defaultStyle.Render("(default: "+o.def+")")
			}
			b.WriteString(line + "\n")
		}
		b.WriteString("\n")
	}

	b.WriteString(dividerStyle.Render(strings.Repeat("─", 80)) + "\n")
	b.WriteString(subStyle.Render("Examples:\n"))
	b.WriteString("  " + flagStyle.Render("claude.exe -d cookies/ -w 8") + "\n")
	b.WriteString("  " + flagStyle.Render("claude.exe -tui") + "\n")
	b.WriteString("  " + flagStyle.Render("claude.exe -d cookies/single.txt -p proxies.txt -m rotating") + "\n\n")

	return b.String()
}

// newScanner returns a tui.ScanFunc that runs one full scan (discover, parse,
// worker pool with file sorting) for a folder. The flag-driven flow and the
// interactive -tui flow share it, so there is a single code path.
func newScanner(proxies []proxy.Proxy, mode string, workers int, timeout time.Duration, endpoint, validDir, invalidDir string) tui.ScanFunc {
	var moveMu sync.Mutex
	moveTo := func(path, targetDir string) string {
		if targetDir == "" {
			return path
		}
		moveMu.Lock()
		defer moveMu.Unlock()
		dst := cookie.UniqueDestPath(targetDir, filepath.Base(path))
		if err := cookie.MoveFile(path, dst); err == nil {
			return dst
		}
		return path
	}

	return func(dir, pattern string) ([]string, []*cookie.CookieFile, <-chan validator.Result, error) {
		paths, err := cookie.DiscoverCookieFiles(dir, pattern)
		if err != nil {
			return nil, nil, nil, err
		}

		// Parse everything up front: fast, local.
		parsed := make([]*cookie.CookieFile, len(paths))
		for i, p := range paths {
			cf, err := cookie.ParseNetscapeFile(p)
			if err != nil {
				cf = &cookie.CookieFile{Path: p, Err: err}
			}
			parsed[i] = cf
		}

		// Proxy selection per file. Sticky pins file i to proxies[i%len];
		// rotating round-robins endpoints per HTTP request via a per-run
		// atomic counter.
		var rr atomic.Uint64
		pickFor := func(i int) func() *proxy.Proxy {
			if len(proxies) == 0 {
				return func() *proxy.Proxy { return nil }
			}
			if mode == "sticky" {
				p := &proxies[i%len(proxies)]
				return func() *proxy.Proxy { return p }
			}
			return func() *proxy.Proxy {
				n := rr.Add(1) - 1
				return &proxies[n%uint64(len(proxies))]
			}
		}

		results := make(chan validator.Result, len(paths))
		jobs := make(chan int, len(paths))
		var wg sync.WaitGroup
		for range workers {
			wg.Add(1)
			go func() {
				defer wg.Done()
				for i := range jobs {
					cf := parsed[i]
					res := validator.CheckCookieFile(context.Background(), cf, pickFor(i), timeout, endpoint)
					if res.Status == validator.StatusValid {
						res.MovedPath = moveTo(cf.Path, validDir)
					} else {
						res.MovedPath = moveTo(cf.Path, invalidDir)
					}
					results <- res
				}
			}()
		}
		go func() {
			for i, cf := range parsed {
				if cf.Err != nil {
					res := validator.Result{
						Path:      cf.Path,
						MovedPath: moveTo(cf.Path, invalidDir),
						Status:    validator.StatusParseError,
						Detail:    cf.Err.Error(),
					}
					results <- res
					continue
				}
				jobs <- i
			}
			close(jobs)
			wg.Wait()
			close(results)
		}()
		return paths, parsed, results, nil
	}
}

func main() {
	flag.Usage = func() {
		fmt.Print(renderColoredHelp())
	}

	var dirFlag, folderFlag, fFlag, dFlag string
	flag.StringVar(&dirFlag, "dir", "", "cookie file or folder to scan recursively (default: cookies/)")
	flag.StringVar(&folderFlag, "folder", "", "alias for -dir")
	flag.StringVar(&fFlag, "f", "", "alias for -dir")
	flag.StringVar(&dFlag, "d", "", "alias for -dir")

	pattern := flag.String("pattern", "*", "base-name glob when -dir is a folder (default: * [all files])")

	var proxiesFlag, proxyFlag, pFlag string
	flag.StringVar(&proxiesFlag, "proxies", "", "proxy list file (used only if it exists)")
	flag.StringVar(&proxyFlag, "proxy", "", "alias for -proxies")
	flag.StringVar(&pFlag, "p", "", "alias for -proxies")

	var proxyModeFlag, modeFlag, mFlag string
	flag.StringVar(&proxyModeFlag, "proxy-mode", "", "sticky or rotating")
	flag.StringVar(&modeFlag, "mode", "", "alias for -proxy-mode")
	flag.StringVar(&mFlag, "m", "", "alias for -proxy-mode")

	var workersFlag, wFlag int
	flag.IntVar(&workersFlag, "workers", 0, "concurrent checks (min 1)")
	flag.IntVar(&wFlag, "w", 0, "alias for -workers")

	var timeoutFlag, tFlag time.Duration
	flag.DurationVar(&timeoutFlag, "timeout", 0, "per-request timeout")
	flag.DurationVar(&tFlag, "t", 0, "alias for -timeout")

	var endpointFlag, eFlag string
	flag.StringVar(&endpointFlag, "endpoint", "", "API base URL")
	flag.StringVar(&eFlag, "e", "", "alias for -endpoint")

	var validDirFlag, validFlag, vFlag string
	flag.StringVar(&validDirFlag, "valid-dir", "", "folder to move valid cookie files to (empty to disable)")
	flag.StringVar(&validFlag, "valid", "", "alias for -valid-dir")
	flag.StringVar(&vFlag, "v", "", "alias for -valid-dir")

	var invalidDirFlag, invalidFlag, iFlag string
	flag.StringVar(&invalidDirFlag, "invalid-dir", "", "folder to move invalid cookie files to (empty to disable)")
	flag.StringVar(&invalidFlag, "invalid", "", "alias for -invalid-dir")
	flag.StringVar(&iFlag, "i", "", "alias for -invalid-dir")

	tuiMode := flag.Bool("tui", false, "interactive mode: pick the cookie folder inside the TUI")
	flag.Parse()

	targetDir := firstNonEmpty(dirFlag, folderFlag, fFlag, dFlag, "cookies")
	proxiesPath := firstNonEmpty(proxiesFlag, proxyFlag, pFlag, "proxies.txt")
	proxyMode := firstNonEmpty(proxyModeFlag, modeFlag, mFlag, "sticky")

	workers := workersFlag
	if workers <= 0 {
		workers = wFlag
	}
	if workers <= 0 {
		workers = 5
	}

	timeout := timeoutFlag
	if timeout <= 0 {
		timeout = tFlag
	}
	if timeout <= 0 {
		timeout = 30 * time.Second
	}

	endpoint := firstNonEmpty(endpointFlag, eFlag, "https://claude.ai")
	validDir := firstNonEmpty(validDirFlag, validFlag, vFlag, "valid")
	invalidDir := firstNonEmpty(invalidDirFlag, invalidFlag, iFlag, "invalid")

	if proxyMode != "sticky" && proxyMode != "rotating" {
		fatal(fmt.Errorf("invalid -proxy-mode %q (want sticky or rotating)", proxyMode))
	}

	// Proxies are optional: a missing proxies.txt is not an error.
	var proxies []proxy.Proxy
	if _, err := os.Stat(proxiesPath); err == nil {
		proxies, err = proxy.LoadProxies(proxiesPath)
		if err != nil {
			fatal(err)
		}
	}

	endpointNorm := strings.TrimRight(endpoint, "/")
	scan := newScanner(proxies, proxyMode, workers, timeout, endpointNorm, validDir, invalidDir)

	if *tuiMode {
		if !isCharDevice(os.Stdout) || !isCharDevice(os.Stdin) {
			fatal(fmt.Errorf("-tui requires an interactive terminal (stdin and stdout on a TTY)"))
		}
		if err := tui.RunInteractive(tui.NewInteractiveModel(scan, endpointNorm, proxyMode, len(proxies), workers)); err != nil {
			fatal(err)
		}
		return
	}

	paths, parsed, results, err := scan(targetDir, *pattern)
	if err != nil {
		if os.IsNotExist(err) && targetDir == "cookies" {
			_ = os.MkdirAll(targetDir, 0o755)
			fmt.Fprintf(os.Stderr, "created %s/ folder; drop your cookie files here\n", targetDir)
			os.Exit(0)
		}
		fatal(err)
	}
	if len(paths) == 0 {
		fmt.Fprintf(os.Stderr, "no cookie files found in %s matching %s\n", targetDir, *pattern)
		os.Exit(0)
	}

	if err := tui.Run(paths, parsed, endpointNorm, proxyMode, len(proxies), workers, results); err != nil {
		fatal(err)
	}
}

func isCharDevice(f *os.File) bool {
	fi, err := f.Stat()
	if err != nil {
		return false
	}
	return (fi.Mode() & os.ModeCharDevice) != 0
}

func firstNonEmpty(vals ...string) string {
	for _, v := range vals {
		if v != "" {
			return v
		}
	}
	return ""
}

func fatal(err error) {
	fmt.Fprintf(os.Stderr, "error: %v\n", err)
	os.Exit(1)
}
