package logging

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"sync"
)

const logFileEnv = "STRMHUB_LOG_FILE"

var activePath struct {
	sync.RWMutex
	value string
}

// OpenAppLog chooses and opens the application log file. Native macOS installs
// keep it beside the LaunchAgent logs; container installs keep using /logs.
func OpenAppLog() (*os.File, string, error) {
	candidates := candidatePaths()
	var failures []error
	var lastPath string
	for _, path := range candidates {
		lastPath = path
		if err := os.MkdirAll(filepath.Dir(path), 0755); err != nil {
			failures = append(failures, fmt.Errorf("create log directory %s: %w", filepath.Dir(path), err))
			continue
		}

		file, err := os.OpenFile(path, os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0644)
		if err != nil {
			failures = append(failures, fmt.Errorf("open log file %s: %w", path, err))
			continue
		}

		setActivePath(path)
		return file, path, nil
	}

	setActivePath(lastPath)
	return nil, lastPath, errors.Join(failures...)
}

// ActivePath returns the exact path selected for the running process.
func ActivePath() string {
	activePath.RLock()
	path := activePath.value
	activePath.RUnlock()
	if path != "" {
		return path
	}
	paths := candidatePaths()
	if len(paths) == 0 {
		return filepath.Join("/logs", "app.log")
	}
	return paths[0]
}

func setActivePath(path string) {
	activePath.Lock()
	activePath.value = path
	activePath.Unlock()
}

func candidatePaths() []string {
	if path := strings.TrimSpace(os.Getenv(logFileEnv)); path != "" {
		return []string{filepath.Clean(path)}
	}

	if runtime.GOOS == "darwin" {
		if home, err := os.UserHomeDir(); err == nil && home != "" {
			return []string{filepath.Join(home, "Library", "Logs", "STRMhub", "app.log"), filepath.Join("/logs", "app.log")}
		}
	}

	return []string{filepath.Join("/logs", "app.log")}
}
