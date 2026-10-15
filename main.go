package main

import (
	"context"
	"flag"
	"fmt"
	"os"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	tea "github.com/charmbracelet/bubbletea"
)

func main() {
	dir := flag.String("dir", ".", "cookie file or folder to scan recursively")
	pattern := flag.String("pattern", "cookie_*.txt", "base-name glob when -dir is a folder")
	proxiesPath := flag.String("proxies", "proxies.txt", "proxy list file (used only if it exists)")
	proxyMode := flag.String("proxy-mode", "sticky", "sticky (pin file to one static proxy) or rotating (round-robin endpoints)")
	workers := flag.Int("workers", 5, "concurrent checks (min 1)")
	timeout := flag.Duration("timeout", 30*time.Second, "per-request timeout")
	endpoint := flag.String("endpoint", "https://claude.ai", "API base URL")
	flag.Parse()

	if *proxyMode != "sticky" && *proxyMode != "rotating" {
		fatal(fmt.Errorf("invalid -proxy-mode %q (want sticky or rotating)", *proxyMode))
	}
	if *workers < 1 {
		*workers = 1
	}

	paths, err := DiscoverCookieFiles(*dir, *pattern)
	if err != nil {
		fatal(err)
	}
	if len(paths) == 0 {
		fmt.Printf("no cookie files match %s in %s\n", *pattern, *dir)
		return
	}

	// Parse everything up front: fast, local.
	parsed := make([]*CookieFile, len(paths))
	for i, p := range paths {
		cf, err := ParseNetscapeFile(p)
		if err != nil {
			cf = &CookieFile{Path: p, Err: err}
		}
		parsed[i] = cf
	}

	// Proxies are optional: a missing proxies.txt is not an error.
	var proxies []Proxy
	if _, err := os.Stat(*proxiesPath); err == nil {
		proxies, err = LoadProxies(*proxiesPath)
		if err != nil {
			fatal(err)
		}
	}

	endpointNorm := strings.TrimRight(*endpoint, "/")

	// Proxy selection per file. Sticky pins file i to proxies[i%len]; rotating
	// round-robins endpoints per HTTP request via a shared atomic counter.
	var rr atomic.Uint64
	pickFor := func(i int) func() *Proxy {
		if len(proxies) == 0 {
			return func() *Proxy { return nil }
		}
		if *proxyMode == "sticky" {
			p := &proxies[i%len(proxies)]
			return func() *Proxy { return p }
		}
		return func() *Proxy {
			n := rr.Add(1)
			return &proxies[(int(n)-1)%len(proxies)]
		}
	}

	results := make(chan Result, len(paths))
	jobs := make(chan int, len(paths))
	var wg sync.WaitGroup
	for range *workers {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for i := range jobs {
				results <- CheckCookieFile(context.Background(), parsed[i], pickFor(i), *timeout, endpointNorm)
			}
		}()
	}
	go func() {
		for i, cf := range parsed {
			if cf.Err == nil { // parse-error rows are already final in the TUI
				jobs <- i
			}
		}
		close(jobs)
		wg.Wait()
		close(results)
	}()

	m := newModel(paths, parsed, endpointNorm, *proxyMode, len(proxies), *workers, results)
	var p *tea.Program
	if m.headless {
		// Piped output: no renderer (no frames on stdout); after the run the
		// complete per-file list plus summary is printed below.
		p = tea.NewProgram(m, tea.WithInput(nil), tea.WithoutRenderer())
	} else {
		p = tea.NewProgram(m)
	}
	final, err := p.Run()
	if err != nil {
		fatal(err)
	}
	if m.headless {
		if fm, ok := final.(model); ok {
			fmt.Print(fm.plainOutput())
		}
	}
}

func fatal(err error) {
	fmt.Fprintln(os.Stderr, "error:", err)
	os.Exit(1)
}
