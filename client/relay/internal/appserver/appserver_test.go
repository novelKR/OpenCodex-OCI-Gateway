package appserver

import (
	"os"
	"testing"
)

func TestIsCodexAppServerIsNarrow(t *testing.T) {
	cases := map[string]bool{
		"/opt/codex/bin/codex app-server":         true,
		"codex --config x=y app-server daemon":    true,
		"/opt/codex-code-mode-host --listen":      true,
		"node worker.js codex app-server":         false,
		"/usr/bin/hermes-codex-bridge app-server": false,
		"codex exec hello":                        false,
	}
	for input, expected := range cases {
		if actual := IsCodexAppServer(input); actual != expected {
			t.Errorf("IsCodexAppServer(%q) = %v, want %v", input, actual, expected)
		}
	}
}

func TestHasCodexHomeRequiresExactEnvironmentIdentity(t *testing.T) {
	expected := "/home/test/.codex-relay"
	cases := []struct {
		name    string
		process Process
		env     []byte
		want    bool
	}{
		{
			name:    "exact home",
			process: Process{PID: 101, Command: "codex app-server daemon"},
			env:     []byte("PATH=/usr/bin\x00CODEX_HOME=/home/test/.codex-relay\x00"),
			want:    true,
		},
		{
			name:    "different home",
			process: Process{PID: 102, Command: "codex app-server daemon"},
			env:     []byte("CODEX_HOME=/home/test/.codex-other\x00"),
			want:    false,
		},
		{
			name:    "missing identity",
			process: Process{PID: 103, Command: "codex app-server daemon"},
			env:     []byte("PATH=/usr/bin\x00"),
			want:    false,
		},
		{
			name:    "duplicate identity is rejected",
			process: Process{PID: 105, Command: "codex app-server daemon"},
			env:     []byte("CODEX_HOME=/home/test/.codex-relay\x00CODEX_HOME=/home/test/.codex-other\x00"),
			want:    false,
		},
		{
			name:    "non appserver cannot spoof home",
			process: Process{PID: 104, Command: "codex exec CODEX_HOME=/home/test/.codex-relay"},
			env:     []byte("CODEX_HOME=/home/test/.codex-relay\x00"),
			want:    false,
		},
	}
	for _, test := range cases {
		t.Run(test.name, func(t *testing.T) {
			got := hasCodexHome(test.process, expected, func(int) ([]byte, error) { return test.env, nil })
			if got != test.want {
				t.Fatalf("hasCodexHome() = %t, want %t", got, test.want)
			}
		})
	}
}

func TestSelectForCodexHomeRetainsUnverifiableCandidates(t *testing.T) {
	expected := "/home/test/.codex-relay"
	candidates := []Process{
		{PID: 101, Command: "codex app-server daemon"},
		{PID: 102, Command: "codex app-server daemon"},
		{PID: 103, Command: "codex app-server daemon"},
	}
	selection := selectForCodexHome(candidates, expected, func(pid int) ([]byte, error) {
		switch pid {
		case 101:
			return []byte("CODEX_HOME=/home/test/.codex-relay\x00"), nil
		case 102:
			return []byte("CODEX_HOME=/home/test/.codex-other\x00"), nil
		default:
			return nil, os.ErrPermission
		}
	})
	if len(selection.Eligible) != 1 || selection.Eligible[0].PID != 101 {
		t.Fatalf("eligible = %#v, want only PID 101", selection.Eligible)
	}
	if len(selection.Unverifiable) != 2 || selection.Unverifiable[0].PID != 102 || selection.Unverifiable[1].PID != 103 {
		t.Fatalf("unverifiable = %#v, want PIDs 102 and 103", selection.Unverifiable)
	}
}
