// Package appserver narrowly identifies native Codex app-server processes for
// idle catalog activation. It never broad-matches arbitrary "codex" processes.
package appserver

import (
	"bufio"
	"bytes"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strconv"
	"strings"
	"syscall"
)

type Process struct {
	PID     int
	Command string
}

// Selection separates positively identified AppServers from candidates whose
// CODEX_HOME could not be proven to match. An activation owner must defer when
// Unverifiable is non-empty: it cannot safely assume that a candidate has
// already consumed the new catalog.
type Selection struct {
	Eligible     []Process
	Unverifiable []Process
}

// ListForCodexHome identifies AppServers owned by the current OS user whose
// process environment proves an exact selected CODEX_HOME. Linux /proc is the
// currently supported identity source. Other platforms fail closed instead of
// inferring a home from an ambiguous command line. A candidate with a missing,
// different, malformed, or unreadable identity remains in Unverifiable rather
// than being silently treated as absent.
func ListForCodexHome(home string) (Selection, error) {
	if !filepath.IsAbs(home) || filepath.Clean(home) != home {
		return Selection{}, errors.New("Codex home must be a clean absolute path")
	}
	if runtime.GOOS != "linux" {
		return Selection{}, errors.New("automatic AppServer restart requires Linux /proc CODEX_HOME identity")
	}
	command := exec.Command("ps", "-x", "-o", "pid=,command=")
	output, err := command.Output()
	if err != nil {
		return Selection{}, fmt.Errorf("list processes: %w", err)
	}
	var candidates []Process
	scanner := bufio.NewScanner(strings.NewReader(string(output)))
	for scanner.Scan() {
		line := strings.TrimSpace(scanner.Text())
		fields := strings.Fields(line)
		if len(fields) < 2 {
			continue
		}
		pid, err := strconv.Atoi(fields[0])
		if err != nil || pid == os.Getpid() {
			continue
		}
		commandLine := strings.TrimSpace(strings.TrimPrefix(line, fields[0]))
		process := Process{PID: pid, Command: commandLine}
		if !IsCodexAppServer(commandLine) {
			continue
		}
		candidates = append(candidates, process)
	}
	if err := scanner.Err(); err != nil {
		return Selection{}, err
	}
	return selectForCodexHome(candidates, home, readProcEnvironment), nil
}

func selectForCodexHome(candidates []Process, expected string, readEnvironment func(int) ([]byte, error)) Selection {
	var result Selection
	for _, process := range candidates {
		if hasCodexHome(process, expected, readEnvironment) {
			result.Eligible = append(result.Eligible, process)
		} else {
			result.Unverifiable = append(result.Unverifiable, process)
		}
	}
	return result
}

func readProcEnvironment(pid int) ([]byte, error) {
	return os.ReadFile(filepath.Join("/proc", strconv.Itoa(pid), "environ"))
}

func hasCodexHome(process Process, expected string, readEnvironment func(int) ([]byte, error)) bool {
	if !IsCodexAppServer(process.Command) {
		return false
	}
	environment, err := readEnvironment(process.PID)
	if err != nil {
		return false
	}
	var home string
	foundHome := false
	for _, entry := range bytes.Split(environment, []byte{0}) {
		value, found := bytes.CutPrefix(entry, []byte("CODEX_HOME="))
		if !found {
			continue
		}
		if foundHome || !filepath.IsAbs(string(value)) {
			return false
		}
		foundHome = true
		home = filepath.Clean(string(value))
	}
	return foundHome && home == expected
}

// IsCodexAppServer permits only a Codex executable whose first command is
// app-server, or the well-known code-mode host executable. It intentionally
// rejects a command where "codex app-server" occurs as later data.
func IsCodexAppServer(commandLine string) bool {
	fields := strings.Fields(strings.TrimSpace(commandLine))
	if len(fields) == 0 {
		return false
	}
	base := strings.ToLower(filepath.Base(strings.Trim(fields[0], "\"'")))
	if strings.HasPrefix(base, "codex-code-mode-host") {
		return true
	}
	if base != "codex" && !strings.HasPrefix(base, "codex-") {
		return false
	}
	for index := 1; index < len(fields); index++ {
		field := fields[index]
		if strings.HasPrefix(field, "-") {
			if optionTakesValue(field) && index+1 < len(fields) {
				index++
			}
			continue
		}
		return field == "app-server"
	}
	return false
}

func optionTakesValue(option string) bool {
	switch option {
	case "--config", "-c", "--profile", "--model", "-m", "--cd", "-C", "--sandbox", "--approval-policy":
		return true
	default:
		return false
	}
}

func Restart(processes []Process) ([]int, error) {
	var signaled []int
	var failures []error
	for _, process := range processes {
		if process.PID <= 1 {
			failures = append(failures, fmt.Errorf("refuse invalid process ID %d", process.PID))
			continue
		}
		if err := syscall.Kill(process.PID, syscall.SIGTERM); err != nil {
			failures = append(failures, fmt.Errorf("SIGTERM %d: %w", process.PID, err))
			continue
		}
		signaled = append(signaled, process.PID)
	}
	return signaled, errors.Join(failures...)
}
