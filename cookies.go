package main

import (
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

// buildCookieHeader joins the parsed cookies the way a browser would send them.
func buildCookieHeader(cookies map[string]string) string {
	parts := make([]string, 0, len(cookies))
	for name, value := range cookies {
		parts = append(parts, name+"="+value)
	}
	return strings.Join(parts, "; ")
}
