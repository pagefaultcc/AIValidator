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
)

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
	dir := flag.String("dir", "cookies", "cookie file or folder to scan recursively (default: cookies/)")
	pattern := flag.String("pattern", "*", "base-name glob when -dir is a folder (default: * [all files])")
	proxiesPath := flag.String("proxies", "proxies.txt", "proxy list file (used only if it exists)")
	proxyMode := flag.String("proxy-mode", "sticky", "sticky (pin file to one static proxy) or rotating (round-robin endpoints)")
	workers := flag.Int("workers", 5, "concurrent checks (min 1)")
	timeout := flag.Duration("timeout", 30*time.Second, "per-request timeout")
	endpoint := flag.String("endpoint", "https://claude.ai", "API base URL")
	validDir := flag.String("valid-dir", "valid", "folder to move valid cookie files to (empty to disable)")
	invalidDir := flag.String("invalid-dir", "invalid", "folder to move invalid cookie files to (empty to disable)")
	tuiMode := flag.Bool("tui", false, "interactive mode: pick the cookie folder inside the TUI")
	flag.Parse()

	if *proxyMode != "sticky" && *proxyMode != "rotating" {
		fatal(fmt.Errorf("invalid -proxy-mode %q (want sticky or rotating)", *proxyMode))
	}
	if *workers < 1 {
		*workers = 1
	}

	// Proxies are optional: a missing proxies.txt is not an error.
	var proxies []proxy.Proxy
	if _, err := os.Stat(*proxiesPath); err == nil {
		proxies, err = proxy.LoadProxies(*proxiesPath)
		if err != nil {
			fatal(err)
		}
	}

	endpointNorm := strings.TrimRight(*endpoint, "/")
	scan := newScanner(proxies, *proxyMode, *workers, *timeout, endpointNorm, *validDir, *invalidDir)

	if *tuiMode {
		if !isCharDevice(os.Stdout) || !isCharDevice(os.Stdin) {
			fatal(fmt.Errorf("-tui requires an interactive terminal (stdin and stdout on a TTY)"))
		}
		if err := tui.RunInteractive(tui.NewInteractiveModel(scan, endpointNorm, *proxyMode, len(proxies), *workers)); err != nil {
			fatal(err)
		}
		return
	}

	paths, parsed, results, err := scan(*dir, *pattern)
	if err != nil {
		if os.IsNotExist(err) && *dir == "cookies" {
			_ = os.MkdirAll(*dir, 0o755)
			fmt.Fprintf(os.Stderr, "created %s/ folder; drop your cookie files here\n", *dir)
			os.Exit(0)
		}
		fatal(err)
	}
	if len(paths) == 0 {
		fmt.Fprintf(os.Stderr, "no cookie files found in %s matching %s\n", *dir, *pattern)
		os.Exit(0)
	}
	if err := tui.Run(paths, parsed, endpointNorm, *proxyMode, len(proxies), *workers, results); err != nil {
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

func fatal(err error) {
	fmt.Fprintln(os.Stderr, "error:", err)
	os.Exit(1)
}
