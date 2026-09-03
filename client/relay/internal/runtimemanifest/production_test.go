package runtimemanifest

import "testing"

func TestLoopbackCanaryCheckerAcceptsOnlyExplicitTLSIPv4Loopback(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	for _, value := range []string{
		"https://127.0.0.1:8443",
		"https://127.0.0.1:1",
	} {
		if _, err := NewLoopbackCanaryChecker(value); err != nil {
			t.Fatalf("NewLoopbackCanaryChecker(%q): %v", value, err)
		}
	}
}

func TestLoopbackCanaryCheckerRejectsGeneralOverrides(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	for _, value := range []string{
		"http://127.0.0.1:8443",
		"https://localhost:8443",
		"https://127.0.0.2:8443",
		"https://127.0.0.1",
		"https://127.0.0.1:8443/",
		"https://127.0.0.1:8443/releases",
		"https://user@127.0.0.1:8443",
		"https://127.0.0.1:8443?x=1",
		"https://127.0.0.1:8443#fragment",
		"not-a-url",
	} {
		t.Run(value, func(t *testing.T) {
			if _, err := NewLoopbackCanaryChecker(value); err == nil {
				t.Fatalf("NewLoopbackCanaryChecker(%q) succeeded", value)
			}
		})
	}
}
