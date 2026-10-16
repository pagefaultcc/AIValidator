package cookie

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
)

// CookieFile represents a parsed cookie file (Netscape or JSON format).
type CookieFile struct {
	Path    string
	Cookies map[string]string // cookie name -> cookie value
	Err     error             // non-nil if file could not be read
}

// ParseCookieFile reads a cookie file, supporting both Netscape format (tab or
// whitespace separated, preserving #HttpOnly_ lines) and JSON exports.
// Optimized with a 512KB limit reader to avoid runaway allocations on corrupt files.
func ParseCookieFile(path string) (*CookieFile, error) {
	f, err := os.Open(path)
	if err != nil {
		return nil, err
	}
	defer f.Close()

	// Read up to 512KB (normal cookie files are < 50KB)
	data, err := io.ReadAll(io.LimitReader(f, 512*1024))
	if err != nil {
		return nil, err
	}

	cf := &CookieFile{
		Path:    path,
		Cookies: make(map[string]string),
	}

	trimmed := bytes.TrimSpace(data)
	if len(trimmed) == 0 {
		return cf, nil
	}

	// 1. JSON export fallback (array of cookie objects or key-value map)
	if trimmed[0] == '[' || trimmed[0] == '{' {
		if parseJSONCookies(trimmed, cf.Cookies) {
			return cf, nil
		}
	}

	// 2. Netscape format line parser
	lines := bytes.Split(data, []byte("\n"))
	for _, rawLine := range lines {
		rawLine = bytes.TrimRight(rawLine, "\r")
		rawLine = bytes.TrimSpace(rawLine)
		if len(rawLine) == 0 {
			continue
		}

		if bytes.HasPrefix(rawLine, []byte("#HttpOnly_")) {
			rawLine = bytes.TrimPrefix(rawLine, []byte("#HttpOnly_"))
		} else if rawLine[0] == '#' {
			continue
		}

		parts := bytes.Split(rawLine, []byte("\t"))
		if len(parts) != 7 {
			parts = bytes.Fields(rawLine)
			if len(parts) != 7 {
				continue
			}
		}

		name := string(parts[5])
		val := string(parts[6])
		if name != "" {
			cf.Cookies[name] = val
		}
	}

	return cf, nil
}

func parseJSONCookies(data []byte, target map[string]string) bool {
	// Try parsing as array of cookie objects: [{"name": "foo", "value": "bar"}, ...]
	var list []map[string]any
	if err := json.Unmarshal(data, &list); err == nil && len(list) > 0 {
		for _, item := range list {
			var name, val string
			if n, ok := item["name"].(string); ok && n != "" {
				name = n
			} else if n, ok := item["Name"].(string); ok && n != "" {
				name = n
			}

			if v := item["value"]; v != nil {
				val = fmt.Sprintf("%v", v)
			} else if v := item["Value"]; v != nil {
				val = fmt.Sprintf("%v", v)
			}

			if name != "" && val != "" {
				target[name] = val
			}
		}
		if len(target) > 0 {
			return true
		}
	}

	// Try parsing as key-value map: {"cookie_name": "cookie_value", ...}
	var dict map[string]any
	if err := json.Unmarshal(data, &dict); err == nil && len(dict) > 0 {
		for k, v := range dict {
			if v != nil {
				target[k] = fmt.Sprintf("%v", v)
			}
		}
		if len(target) > 0 {
			return true
		}
	}

	return false
}

// DiscoverCookieFiles discovers candidate cookie files (used for testing and small scans).
func DiscoverCookieFiles(dir, pattern string) ([]string, error) {
	info, err := os.Stat(dir)
	if err != nil {
		return nil, err
	}
	if !info.IsDir() {
		return []string{dir}, nil
	}

	allowedExts := map[string]bool{
		".txt":     true,
		".cookie":  true,
		".cookies": true,
		".json":    true,
		"":         true,
	}

	ignoredExts := map[string]bool{
		".py":  true,
		".go":  true,
		".exe": true,
		".dll": true,
		".so":  true,
		".mod": true,
		".sum": true,
		".md":  true,
	}

	var out []string
	err = filepath.WalkDir(dir, func(path string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		name := d.Name()
		if d.IsDir() {
			if strings.EqualFold(name, "valid") || strings.EqualFold(name, "invalid") {
				return filepath.SkipDir
			}
			if strings.HasPrefix(name, ".") && !strings.HasPrefix(pattern, ".") {
				return filepath.SkipDir
			}
			return nil
		}

		if strings.HasPrefix(name, ".") && !strings.HasPrefix(pattern, ".") {
			return nil
		}

		ext := strings.ToLower(filepath.Ext(name))
		if ignoredExts[ext] {
			return nil
		}

		if pattern != "" && pattern != "*" {
			if ok, _ := filepath.Match(pattern, name); ok {
				out = append(out, path)
			}
			return nil
		}

		if allowedExts[ext] {
			out = append(out, path)
		}
		return nil
	})

	if err != nil {
		return nil, err
	}
	return out, nil
}

// FastCountCookieFiles quickly counts matching cookie files on disk without
// opening or reading them. On 100K files, this completes in ~50-100ms.
func FastCountCookieFiles(dir, pattern string) int64 {
	info, err := os.Stat(dir)
	if err != nil {
		return 0
	}
	if !info.IsDir() {
		return 1
	}

	allowedExts := map[string]bool{
		".txt":     true,
		".cookie":  true,
		".cookies": true,
		".json":    true,
		"":         true,
	}

	ignoredExts := map[string]bool{
		".py":  true,
		".go":  true,
		".exe": true,
		".dll": true,
		".so":  true,
		".mod": true,
		".sum": true,
		".md":  true,
	}

	var count int64
	_ = filepath.WalkDir(dir, func(path string, d fs.DirEntry, err error) error {
		if err != nil {
			return nil
		}
		name := d.Name()
		if d.IsDir() {
			if strings.EqualFold(name, "valid") || strings.EqualFold(name, "invalid") {
				return filepath.SkipDir
			}
			if strings.HasPrefix(name, ".") && !strings.HasPrefix(pattern, ".") {
				return filepath.SkipDir
			}
			return nil
		}

		if strings.HasPrefix(name, ".") && !strings.HasPrefix(pattern, ".") {
			return nil
		}

		ext := strings.ToLower(filepath.Ext(name))
		if ignoredExts[ext] {
			return nil
		}

		if pattern != "" && pattern != "*" {
			if ok, _ := filepath.Match(pattern, name); ok {
				count++
			}
			return nil
		}

		if allowedExts[ext] {
			count++
		}
		return nil
	})
	return count
}

// StreamCookieFiles walks dir and streams matching cookie file paths one by one
// into out channel. It begins streaming immediately without buffering all files
// into memory, making it scale to 100K+ files with minimal RAM.
func StreamCookieFiles(ctx context.Context, dir, pattern string, out chan<- string, totalDiscovered *atomic.Int64, discoveryDone *atomic.Bool) error {
	defer func() {
		if discoveryDone != nil {
			discoveryDone.Store(true)
		}
		close(out)
	}()

	info, err := os.Stat(dir)
	if err != nil {
		return err
	}
	if !info.IsDir() {
		out <- dir
		if totalDiscovered != nil {
			totalDiscovered.Add(1)
		}
		return nil
	}

	allowedExts := map[string]bool{
		".txt":     true,
		".cookie":  true,
		".cookies": true,
		".json":    true,
		"":         true,
	}

	ignoredExts := map[string]bool{
		".py":  true,
		".go":  true,
		".exe": true,
		".dll": true,
		".so":  true,
		".mod": true,
		".sum": true,
		".md":  true,
	}

	return filepath.WalkDir(dir, func(path string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		select {
		case <-ctx.Done():
			return ctx.Err()
		default:
		}

		name := d.Name()
		if d.IsDir() {
			if strings.EqualFold(name, "valid") || strings.EqualFold(name, "invalid") {
				return filepath.SkipDir
			}
			if strings.HasPrefix(name, ".") && !strings.HasPrefix(pattern, ".") {
				return filepath.SkipDir
			}
			return nil
		}

		if strings.HasPrefix(name, ".") && !strings.HasPrefix(pattern, ".") {
			return nil
		}

		ext := strings.ToLower(filepath.Ext(name))
		if ignoredExts[ext] {
			return nil
		}

		if pattern != "" && pattern != "*" {
			if ok, _ := filepath.Match(pattern, name); ok {
				out <- path
				if totalDiscovered != nil {
					totalDiscovered.Add(1)
				}
			}
			return nil
		}

		if allowedExts[ext] {
			out <- path
			if totalDiscovered != nil {
				totalDiscovered.Add(1)
			}
		}
		return nil
	})
}

// BuildCookieHeader formats cookies map into a standard HTTP Cookie header string.
func BuildCookieHeader(cookies map[string]string) string {
	if len(cookies) == 0 {
		return ""
	}
	parts := make([]string, 0, len(cookies))
	for name, val := range cookies {
		parts = append(parts, name+"="+val)
	}
	return strings.Join(parts, "; ")
}

// UniqueDestPath generates a collision-free file path in targetDir by appending
// _1, _2, etc. if a file with the same name already exists.
func UniqueDestPath(targetDir, baseName string) string {
	target := filepath.Join(targetDir, baseName)
	if _, err := os.Stat(target); os.IsNotExist(err) {
		return target
	}

	ext := filepath.Ext(baseName)
	stem := strings.TrimSuffix(baseName, ext)
	for i := 1; ; i++ {
		candidate := filepath.Join(targetDir, fmt.Sprintf("%s_%d%s", stem, i, ext))
		if _, err := os.Stat(candidate); os.IsNotExist(err) {
			return candidate
		}
	}
}

// MoveFile moves a file safely across filesystems/volumes with copy+delete fallback.
func MoveFile(src, dst string) error {
	cleanSrc := filepath.Clean(src)
	cleanDst := filepath.Clean(dst)
	if cleanSrc == cleanDst {
		return nil
	}

	if err := os.MkdirAll(filepath.Dir(cleanDst), 0o755); err != nil {
		return err
	}

	// Try atomic rename first
	if err := os.Rename(cleanSrc, cleanDst); err == nil {
		return nil
	}

	// Fallback to copy and delete (Windows file-lock / cross-drive compatibility)
	data, err := os.ReadFile(cleanSrc)
	if err != nil {
		return err
	}
	if err := os.WriteFile(cleanDst, data, 0o644); err != nil {
		return err
	}
	_ = os.Remove(cleanSrc)
	return nil
}
