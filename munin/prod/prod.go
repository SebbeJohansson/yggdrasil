// Package prod is Munin's source for production health URLs.
//
// Settings (in munin.env, with prod in MUNIN_SOURCES):
//
//	MUNIN_PROD_URLS  the URLs to probe: https://a.example/health,https://b.example/
//
// One check per URL, raised on Gjallar as prod:<url> (critical, so it reaches
// the phone through Hermóðr). It fails on a transport error or any status
// outside 200 to 399, and passes again once the URL does.
package prod

import (
	"context"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"
	"time"

	"yggdrasil/internal/alert"
	"yggdrasil/internal/watch"
	"yggdrasil/munin/source"
)

// Name is the source's name in MUNIN_SOURCES.
const Name = "prod"

const every = time.Minute

// Source probes each URL.
type Source struct {
	URLs []string
	HTTP *http.Client
}

// New is the source.Factory.
func New(settings source.Settings) (source.Source, error) {
	var urls []string
	for _, raw := range strings.Split(settings("URLS"), ",") {
		raw = strings.TrimSpace(raw)
		if raw == "" {
			continue
		}
		if parsed, err := url.Parse(raw); err != nil || (parsed.Scheme != "http" && parsed.Scheme != "https") || parsed.Host == "" {
			return nil, fmt.Errorf("MUNIN_PROD_URLS: %q is not an http(s) URL", raw)
		}
		urls = append(urls, raw)
	}
	if len(urls) == 0 {
		return nil, fmt.Errorf("MUNIN_PROD_URLS: name the URLs to probe")
	}
	return &Source{URLs: urls, HTTP: &http.Client{Timeout: 15 * time.Second}}, nil
}

func (s *Source) Name() string         { return Name }
func (s *Source) Every() time.Duration { return every }

// Probe is one URL as last seen.
type Probe struct {
	URL    string `json:"url"`
	OK     bool   `json:"ok"`
	Status int    `json:"status,omitempty"`
	Error  string `json:"error,omitempty"`
}

// Poll probes every URL once.
func (s *Source) Poll(ctx context.Context, _ string) source.Report {
	var report source.Report
	var probes []Probe
	down := 0
	for _, u := range s.URLs {
		probe := Probe{URL: u}
		req, err := http.NewRequestWithContext(ctx, http.MethodGet, u, nil)
		if err == nil {
			var resp *http.Response
			if resp, err = s.HTTP.Do(req); err == nil {
				_, _ = io.Copy(io.Discard, io.LimitReader(resp.Body, 1<<20))
				resp.Body.Close()
				probe.Status = resp.StatusCode
				if resp.StatusCode >= 400 {
					err = fmt.Errorf("status %d", resp.StatusCode)
				}
			}
		}
		check := watch.Result{Key: u, OK: err == nil, Severity: alert.Critical, Title: "Production is unhealthy: " + u}
		if err != nil {
			probe.Error = err.Error()
			check.Detail = err.Error()
			down++
		}
		probe.OK = err == nil
		report.Checks = append(report.Checks, check)
		probes = append(probes, probe)
	}
	report.Data = probes
	report.Up = down == 0
	if down > 0 {
		report.Detail = fmt.Sprintf("%d of %d URLs failing", down, len(s.URLs))
	} else {
		report.Detail = fmt.Sprintf("%d URLs healthy", len(s.URLs))
	}
	return report
}
