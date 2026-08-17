package config

import (
	"bufio"
	"log"
	"os"
	"path/filepath"
	"strings"
)

// LoadDotEnv reads environment overrides from `.env` (and, if present,
// `.env.local`, which takes precedence over `.env`). It searches a few
// candidate directories so the file is found regardless of the process working
// directory: the current directory, the executable's directory, and its parent
// (the binary usually lives in ./bin). Keys already present in the real process
// environment always win, so CI or container-injected env is never clobbered by
// a file.
//
// Only a deliberately small, safe subset of dotenv is supported: KEY=VALUE
// lines, `#` comments, blank lines, and optional single/double quoting of the
// value. No variable expansion or shell interpolation is performed — these files
// hold connection secrets, so parsing stays dumb and predictable.
func LoadDotEnv() error {
	// Snapshot the real process environment so neither file can override it,
	// while still letting .env.local override .env.
	processKeys := map[string]bool{}
	for _, kv := range os.Environ() {
		if i := strings.Index(kv, "="); i >= 0 {
			processKeys[kv[:i]] = true
		}
	}

	var loaded []string
	for _, name := range []string{".env", ".env.local"} {
		found := false
		for _, dir := range candidateDirs() {
			p := filepath.Join(dir, name)
			if _, err := os.Stat(p); err != nil {
				continue // not in this candidate dir; try the next
			}
			if err := loadDotEnvFile(p, processKeys); err != nil {
				return err
			}
			loaded = append(loaded, p)
			found = true
			break // this file was found at the highest-priority candidate dir
		}
		if !found {
			log.Printf("LoadDotEnv: 未找到 %s（跳过，不影响启动）", name)
		}
	}
	if len(loaded) > 0 {
		log.Printf("LoadDotEnv: 已加载环境变量文件: %s", strings.Join(loaded, ", "))
	}
	return nil
}

// candidateDirs returns the directories to search for dotenv files, in priority
// order: the current working directory first, then the directory holding the
// executable and its parent (so ./bin/server works from anywhere).
func candidateDirs() []string {
	dirs := []string{"."}
	if exe, err := os.Executable(); err == nil {
		exeDir := filepath.Dir(exe)
		dirs = append(dirs, exeDir, filepath.Dir(exeDir))
	}
	return dirs
}

// loadDotEnvFile parses a single dotenv file. Keys already present in
// processKeys (the real environment) are skipped; everything else is exported.
func loadDotEnvFile(path string, processKeys map[string]bool) error {
	f, err := os.Open(path)
	if err != nil {
		if os.IsNotExist(err) {
			return nil
		}
		return err
	}
	defer f.Close()

	sc := bufio.NewScanner(f)
	for sc.Scan() {
		line := strings.TrimSpace(sc.Text())
		if line == "" || strings.HasPrefix(line, "#") {
			continue
		}
		key, val, ok := splitDotEnvKV(line)
		if !ok {
			continue
		}
		if processKeys[key] {
			continue // real environment wins
		}
		_ = os.Setenv(key, val)
	}
	return sc.Err()
}

// splitDotEnvKV splits a "KEY=VALUE" line, stripping an optional matching pair
// of surrounding single or double quotes from the value.
func splitDotEnvKV(line string) (string, string, bool) {
	idx := strings.Index(line, "=")
	if idx < 0 {
		return "", "", false
	}
	key := strings.TrimSpace(line[:idx])
	val := strings.TrimSpace(line[idx+1:])
	if key == "" {
		return "", "", false
	}
	if len(val) >= 2 {
		if (val[0] == '"' && val[len(val)-1] == '"') ||
			(val[0] == '\'' && val[len(val)-1] == '\'') {
			val = val[1 : len(val)-1]
		}
	}
	return key, val, true
}
