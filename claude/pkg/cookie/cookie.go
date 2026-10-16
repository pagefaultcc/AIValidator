package cookie

import (
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"time"
)

// CookieFile is a parsed Netscape-format cookie file.
type CookieFile struct {
	Path              string
	Cookies           map[string]string // name -> value, flat: the validator talks to one host
	SessionKeyExpires time.Time         // expiry column of the sessionKey cookie; zero if absent/unparseable
	Err               error             // non-nil if the file could not be read at all
}

// ParseNetscapeFile reads a Netscape cookie file. Malformed individual lines are
// ignored (the rest of the file still counts); only an unreadable file is an error.
func ParseNetscapeFile(path string) (*CookieFile, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}
	cf := &CookieFile{Path: path, Cookies: map[string]string{}}
	for _, line := range strings.Split(string(data), "\n") {
		line = strings.TrimRight(line, "\r")
		if line == "" {
			continue
		}
		// #HttpOnly_ lines are data lines; other # lines are comments.
		if strings.HasPrefix(line, "#HttpOnly_") {
			line = strings.TrimPrefix(line, "#HttpOnly_")
		} else if strings.HasPrefix(line, "#") {
			continue
		}
		fields := strings.Split(line, "\t")
		if len(fields) != 7 {
			continue // domain, includeSubdomains, path, secure, expiry, name, value
		}
		name, value, expiry := fields[5], fields[6], fields[4]
		cf.Cookies[name] = value
		if name == "sessionKey" {
			if sec, err := strconv.ParseInt(expiry, 10, 64); err == nil {
				cf.SessionKeyExpires = time.Unix(sec, 0)
			}
		}
	}
	return cf, nil
}

// DiscoverCookieFiles returns the files to validate: dir itself if it is a
// regular file, otherwise every regular file under dir whose base name matches pattern.
func DiscoverCookieFiles(dir, pattern string) ([]string, error) {
	info, err := os.Stat(dir)
	if err != nil {
		return nil, err
	}
	if !info.IsDir() {
		return []string{dir}, nil
	}
	var out []string
	err = filepath.WalkDir(dir, func(path string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if d.IsDir() {
			name := d.Name()
			if name == "valid" || name == "invalid" {
				return filepath.SkipDir
			}
			if strings.HasPrefix(name, ".") && !strings.HasPrefix(pattern, ".") {
				return filepath.SkipDir
			}
			return nil
		}
		if strings.HasPrefix(d.Name(), ".") && !strings.HasPrefix(pattern, ".") {
			return nil
		}
		if pattern == "" || pattern == "*" {
			out = append(out, path)
			return nil
		}
		if ok, _ := filepath.Match(pattern, d.Name()); ok {
			out = append(out, path)
		}
		return nil
	})
	if err != nil {
		return nil, err
	}
	return out, nil
}

// BuildCookieHeader joins the parsed cookies the way a browser would send them.
func BuildCookieHeader(cookies map[string]string) string {
	parts := make([]string, 0, len(cookies))
	for name, value := range cookies {
		parts = append(parts, name+"="+value)
	}
	return strings.Join(parts, "; ")
}

// UniqueDestPath returns a unique path in dir for base name.
// If dir/base exists, it appends _1, _2, etc. before the file extension.
func UniqueDestPath(dir, base string) string {
	target := filepath.Join(dir, base)
	if _, err := os.Stat(target); os.IsNotExist(err) {
		return target
	}
	ext := filepath.Ext(base)
	stem := strings.TrimSuffix(base, ext)
	for i := 1; ; i++ {
		name := fmt.Sprintf("%s_%d%s", stem, i, ext)
		target = filepath.Join(dir, name)
		if _, err := os.Stat(target); os.IsNotExist(err) {
			return target
		}
	}
}

// MoveFile moves a file from src to dst, creating any necessary parent directories.
// It uses os.Rename where possible, falling back to copy+delete for cross-device
// or file-lock compatibility.
func MoveFile(src, dst string) error {
	cleanSrc := filepath.Clean(src)
	cleanDst := filepath.Clean(dst)
	if cleanSrc == cleanDst {
		return nil
	}
	if err := os.MkdirAll(filepath.Dir(cleanDst), 0o755); err != nil {
		return err
	}
	if err := os.Rename(cleanSrc, cleanDst); err == nil {
		return nil
	}
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
