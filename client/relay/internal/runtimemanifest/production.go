package runtimemanifest

import (
	"context"
	"errors"
	"net/http"
	"net/url"
	"strings"
	"time"
)

func NewProductionChecker() (*Checker, error) {
	return newProductionChecker(ProductionAPIBaseURL, allowedRuntimeRedirect)
}

// NewLoopbackCanaryChecker is a deliberately narrow test seam used only by
// the Apple lifecycle canary binary. Production callers must use
// NewProductionChecker. The canary base must be an explicit TLS loopback
// endpoint and redirects are never followed, so a compromised fixture cannot
// turn this into a general release-source override.
func NewLoopbackCanaryChecker(apiBaseURL string) (*Checker, error) {
	base, err := url.Parse(apiBaseURL)
	if err != nil || base.Scheme != "https" || base.Hostname() != "127.0.0.1" || base.Port() == "" ||
		base.Path != "" || base.RawPath != "" || base.RawQuery != "" || base.Fragment != "" || base.User != nil {
		return nil, errors.New("runtime canary API base URL is invalid")
	}
	return newProductionChecker(apiBaseURL, func(*url.URL) bool { return false })
}

func newProductionChecker(apiBaseURL string, redirectAllowed func(*url.URL) bool) (*Checker, error) {
	cache, err := ProductionCheckCache()
	if err != nil {
		return nil, err
	}
	client := &http.Client{
		Timeout: 12 * time.Second,
		CheckRedirect: func(request *http.Request, previous []*http.Request) error {
			if len(previous) >= 4 || redirectAllowed == nil || !redirectAllowed(request.URL) {
				return errors.New("runtime release redirect is not allowed")
			}
			return nil
		},
	}
	return NewChecker(CheckerConfig{
		HTTPClient: client,
		Cache:      cache,
		APIBaseURL: apiBaseURL,
		Repository: ProductionSourceRepo,
		Now:        time.Now,
		Sleep: func(ctx context.Context, duration time.Duration) error {
			timer := time.NewTimer(max(0, duration))
			defer timer.Stop()
			select {
			case <-ctx.Done():
				return ctx.Err()
			case <-timer.C:
				return nil
			}
		},
	})
}

func allowedRuntimeRedirect(target *url.URL) bool {
	if target == nil || target.Scheme != "https" || target.User != nil || target.Fragment != "" {
		return false
	}
	switch strings.ToLower(target.Hostname()) {
	case "api.github.com", "github.com", "objects.githubusercontent.com", "release-assets.githubusercontent.com", "github-releases.githubusercontent.com":
		return true
	default:
		return false
	}
}
