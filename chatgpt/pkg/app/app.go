package app

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

	"github.com/charmbracelet/lipgloss"

	"chatgpt-validator/pkg/cookie"
	"chatgpt-validator/pkg/proxy"
	"chatgpt-validator/pkg/tui"
	"chatgpt-validator/pkg/validator"
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
	b.WriteString(titleStyle.Render("◈ CHATGPT SESSION & COOKIE VALIDATOR ") + badgeStyle.Render("[GO EDITION]") + "\n")
	b.WriteString(subStyle.Render("  High-speed multi-threaded session validator with DNS/network protection") + "\n\n")

	b.WriteString(sectionStyle.Render("USAGE:") + "\n")
	b.WriteString("  " + flagStyle.Render("chatgpt.exe") + " " + typeStyle.Render("[options...]") + "\n\n")

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
					desc:  "Cookie file or directory to scan recursively",
					def:   "cookies/ (or F:\\scrape\\cgpt 2)",
				},
				{
					flags: "-pattern",
					val:   "<glob>",
					desc:  "File base-name glob when target is a folder",
					def:   "*",
				},
			},
		},
		{
			name: "CONCURRENCY & TIMINGS",
			opts: []opt{
				{
					flags: "-w, -workers",
					val:   "<int>",
					desc:  "Number of concurrent worker threads",
					def:   "8",
				},
				{
					flags: "-t, -timeout",
					val:   "<duration>",
					desc:  "Per-request network timeout (e.g. 10s, 30s, 1m)",
					def:   "15s",
				},
				{
					flags: "-e, -endpoint",
					val:   "<url>",
					desc:  "OpenAI ChatGPT base URL",
					def:   "https://chatgpt.com",
				},
			},
		},
		{
			name: "PROXY CONFIGURATION",
			opts: []opt{
				{
					flags: "-p, -proxy, -proxies",
					val:   "<file|str>",
					desc:  "Proxy candidate file or single proxy string",
					def:   "proxies.txt (if present)",
				},
				{
					flags: "-m, -mode, -proxy-mode",
					val:   "<mode>",
					desc:  "sticky (pin file to 1 proxy) or rotating (round-robin)",
					def:   "sticky",
				},
				{
					flags: "-ncp, -no-check-proxies",
					val:   "",
					desc:  "Skip pre-flight proxy health connectivity check",
					def:   "false",
				},
			},
		},
		{
			name: "OUTPUT DIRECTORIES",
			opts: []opt{
				{
					flags: "-v, -valid, -valid-dir",
					val:   "<folder>",
					desc:  "Directory to move valid session files to (empty disables)",
					def:   "valid",
				},
				{
					flags: "-i, -invalid, -invalid-dir",
					val:   "<folder>",
					desc:  "Directory to move dead/expired files to (empty disables)",
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
			// Pad flagPart for clean column alignment
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
	b.WriteString("  " + flagStyle.Render("chatgpt.exe -d cookies/ -w 12") + "\n")
	b.WriteString("  " + flagStyle.Render("chatgpt.exe -d \"F:\\scrape\\cgpt 2\" -p proxies.txt -m rotating") + "\n")
	b.WriteString("  " + flagStyle.Render("chatgpt.exe -d cookies/single.txt -p http://user:pass@host:port") + "\n\n")

	return b.String()
}

// Run parses command line flags and executes the validator workflow.
func Run() {
	flag.Usage = func() {
		fmt.Print(renderColoredHelp())
	}

	// Target directory aliases
	var dirFlag, folderFlag, fFlag, dFlag string
	flag.StringVar(&dirFlag, "dir", "", "cookie file or folder to scan")
	flag.StringVar(&folderFlag, "folder", "", "alias for -dir")
	flag.StringVar(&fFlag, "f", "", "alias for -dir")
	flag.StringVar(&dFlag, "d", "", "alias for -dir")

	pattern := flag.String("pattern", "*", "base-name glob when dir is a folder")

	// Proxies aliases
	var proxiesFlag, proxyFlag, pFlag string
	flag.StringVar(&proxiesFlag, "proxies", "", "proxy string or list file")
	flag.StringVar(&proxyFlag, "proxy", "", "alias for -proxies")
	flag.StringVar(&pFlag, "p", "", "alias for -proxies")

	var ncpFlag, noCheckProxiesFlag bool
	flag.BoolVar(&noCheckProxiesFlag, "no-check-proxies", false, "skip pre-flight proxy health check")
	flag.BoolVar(&ncpFlag, "ncp", false, "alias for -no-check-proxies")

	var proxyModeFlag, modeFlag, mFlag string
	flag.StringVar(&proxyModeFlag, "proxy-mode", "", "proxy mode (sticky or rotating)")
	flag.StringVar(&modeFlag, "mode", "", "alias for -proxy-mode")
	flag.StringVar(&mFlag, "m", "", "alias for -proxy-mode")

	// Workers aliases
	var workersFlag, wFlag int
	flag.IntVar(&workersFlag, "workers", 0, "concurrent checks")
	flag.IntVar(&wFlag, "w", 0, "alias for -workers")

	// Timeout aliases
	var timeoutFlag, tFlag time.Duration
	flag.DurationVar(&timeoutFlag, "timeout", 0, "per-request timeout")
	flag.DurationVar(&tFlag, "t", 0, "alias for -timeout")

	// Endpoint aliases
	var endpointFlag, eFlag string
	flag.StringVar(&endpointFlag, "endpoint", "", "API base URL")
	flag.StringVar(&eFlag, "e", "", "alias for -endpoint")

	// Output directory aliases
	var validDirFlag, validFlag, vFlag string
	flag.StringVar(&validDirFlag, "valid-dir", "", "folder for valid cookie files")
	flag.StringVar(&validFlag, "valid", "", "alias for -valid-dir")
	flag.StringVar(&vFlag, "v", "", "alias for -valid-dir")

	var invalidDirFlag, invalidFlag, iFlag string
	flag.StringVar(&invalidDirFlag, "invalid-dir", "", "folder for invalid cookie files")
	flag.StringVar(&invalidFlag, "invalid", "", "alias for -invalid-dir")
	flag.StringVar(&iFlag, "i", "", "alias for -invalid-dir")

	flag.Parse()

	// Resolve dir
	targetDir := firstNonEmpty(dirFlag, folderFlag, fFlag, dFlag)
	if targetDir == "" {
		if _, err := os.Stat("cookies"); err == nil {
			targetDir = "cookies"
		} else if _, err := os.Stat(`F:\scrape\cgpt 2`); err == nil {
			targetDir = `F:\scrape\cgpt 2`
		} else {
			targetDir = "cookies"
		}
	}

	// Resolve workers
	workers := workersFlag
	if workers <= 0 {
		workers = wFlag
	}
	if workers <= 0 {
		workers = 8
	}

	// Resolve timeout
	timeout := timeoutFlag
	if timeout <= 0 {
		timeout = tFlag
	}
	if timeout <= 0 {
		timeout = 15 * time.Second
	}

	// Resolve endpoint
	endpoint := firstNonEmpty(endpointFlag, eFlag, "https://chatgpt.com")

	// Resolve proxy mode
	proxyMode := firstNonEmpty(proxyModeFlag, modeFlag, mFlag, "sticky")
	if proxyMode != "sticky" && proxyMode != "rotating" {
		fatal(fmt.Errorf("invalid proxy mode %q (want sticky or rotating)", proxyMode))
	}

	// Resolve no-check-proxies
	skipProxyCheck := noCheckProxiesFlag || ncpFlag

	// Resolve output dirs
	validDir := firstNonEmpty(validDirFlag, validFlag, vFlag, "valid")
	invalidDir := firstNonEmpty(invalidDirFlag, invalidFlag, iFlag, "invalid")

	// Resolve proxies input
	proxyInput := firstNonEmpty(proxiesFlag, proxyFlag, pFlag)
	if proxyInput == "" {
		if _, err := os.Stat("proxies.txt"); err == nil {
			proxyInput = "proxies.txt"
		}
	}

	// Ensure target directory or file exists
	if _, err := os.Stat(targetDir); err != nil {
		if os.IsNotExist(err) && targetDir == "cookies" {
			_ = os.MkdirAll(targetDir, 0o755)
			fmt.Fprintf(os.Stderr, "created %s/ folder; drop your cookie files here\n", targetDir)
			os.Exit(0)
		}
		fatal(err)
	}

	// Load & verify proxies
	var proxies []proxy.Proxy
	if proxyInput != "" {
		loaded, err := proxy.LoadProxies(proxyInput)
		if err != nil {
			fmt.Fprintf(os.Stderr, "warning: could not load proxies from %q: %v\n", proxyInput, err)
		} else {
			if !skipProxyCheck && len(loaded) > 0 {
				ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
				proxies = proxy.VerifyProxies(ctx, loaded, 16, 5*time.Second, "")
				cancel()
			} else {
				proxies = loaded
			}
		}
	}

	endpointNorm := strings.TrimRight(endpoint, "/")

	var rr atomic.Uint64
	pickFor := func(i int) func() *proxy.Proxy {
		if len(proxies) == 0 {
			return func() *proxy.Proxy { return nil }
		}
		if proxyMode == "sticky" {
			p := &proxies[i%len(proxies)]
			return func() *proxy.Proxy { return p }
		}
		return func() *proxy.Proxy {
			n := rr.Add(1) - 1
			return &proxies[n%uint64(len(proxies))]
		}
	}

	var moveMu sync.Mutex
	moveTo := func(path, targetFolder string) string {
		if targetFolder == "" {
			return path
		}
		moveMu.Lock()
		defer moveMu.Unlock()
		dst := cookie.UniqueDestPath(targetFolder, filepath.Base(path))
		if err := cookie.MoveFile(path, dst); err == nil {
			return dst
		}
		return path
	}

	// Stream files: producer walks disk, workers consume and parse on-demand
	pathsChan := make(chan string, 1024)
	resultsChan := make(chan validator.Result, 1024)
	var totalDiscovered atomic.Int64
	var discoveryDone atomic.Bool

	go func() {
		_ = cookie.StreamCookieFiles(context.Background(), targetDir, *pattern, pathsChan, &totalDiscovered, &discoveryDone)
	}()

	var wg sync.WaitGroup
	for workerID := range workers {
		wg.Add(1)
		go func(wid int) {
			defer wg.Done()
			for p := range pathsChan {
				cf, err := cookie.ParseCookieFile(p)
				if err != nil {
					cf = &cookie.CookieFile{Path: p, Err: err}
				}
				res := validator.CheckCookieFile(context.Background(), cf, pickFor(wid), timeout, endpointNorm)
				switch res.Status {
				case validator.StatusValid:
					res.MovedPath = moveTo(cf.Path, validDir)
				case validator.StatusInvalid, validator.StatusExpired:
					res.MovedPath = moveTo(cf.Path, invalidDir)
				default:
					res.MovedPath = cf.Path
				}
				resultsChan <- res
			}
		}(workerID)
	}

	go func() {
		wg.Wait()
		close(resultsChan)
	}()

	if err := tui.Run(endpointNorm, proxyMode, len(proxies), workers, &totalDiscovered, &discoveryDone, resultsChan); err != nil {
		fatal(err)
	}
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
