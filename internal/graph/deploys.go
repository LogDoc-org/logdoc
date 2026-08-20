package graph

import (
	"context"
	"log/slog"
	"regexp"
	"sync"
	"time"

	"github.com/LogDoc-org/logdoc/internal/model"
)

// Deploy markers (A1): version changes of a service detected from its own
// logs — an explicit version field, or a deploy-ish message mentioning a
// version. The marker lands on the service card timeline next to whatever
// happened right after it ("2.3.1 deployed, 2s later — the first errors").

// Deploy — one detected deployment/version change.
type Deploy struct {
	App     string    `json:"app"`
	Version string    `json:"version"`
	Ts      time.Time `json:"ts"`
}

// DeployStore persists deploy markers (implemented by the graph SQLite store).
type DeployStore interface {
	// InsertDeploys appends markers, skipping any whose version equals the
	// latest stored version of the same app (restart-safe dedup).
	InsertDeploys(ctx context.Context, tenantID string, deploys []Deploy) error
	// Deploys returns markers newest-first; empty app = all apps.
	Deploys(ctx context.Context, tenantID, app string, since time.Time, limit int) ([]Deploy, error)
}

// versionFields — explicit version-carrying fields, strongest signal.
var versionFields = []string{
	"version", "app_version", "service_version", "service.version",
	"build_version", "release",
}

// deployMsgRe — a deploy-ish verb followed by a version-looking token.
// The verb list is deliberately narrow ("starting" alone is too noisy:
// "started in 1.2 seconds").
var deployMsgRe = regexp.MustCompile(
	`(?i)\b(?:deploy(?:ed|ing)?|releas(?:ed|ing)|rolled\s+out|rolling\s+out|upgrad(?:ed|ing))\b[^\n]*?\bv?(\d+\.\d+(?:\.\d+)?(?:[-+][0-9A-Za-z.\-]+)?)\b`)

// versionMsgRe — "version: 2.3.1" / "version=v2.3.1" anywhere in the message.
var versionMsgRe = regexp.MustCompile(
	`(?i)\bversion\s*[:=]?\s*v?(\d+\.\d+(?:\.\d+)?(?:[-+][0-9A-Za-z.\-]+)?)\b`)

// maxTrackedApps bounds the last-version map.
const maxTrackedApps = 10_000

// DeployDetector consumes the entry stream (as part of the ingest fanout),
// detects version changes per service and flushes markers to the store
// asynchronously.
type DeployDetector struct {
	store DeployStore

	mu      sync.Mutex
	last    map[nodeKey]string // last observed version per service
	pending map[string][]Deploy

	stop chan struct{}
	done chan struct{}
}

// NewDeployDetector creates the detector and starts its flush loop.
func NewDeployDetector(store DeployStore) *DeployDetector {
	d := &DeployDetector{
		store:   store,
		last:    make(map[nodeKey]string),
		pending: make(map[string][]Deploy),
		stop:    make(chan struct{}),
		done:    make(chan struct{}),
	}
	go d.loop()
	return d
}

// Append consumes one entry; maps and a mutex only, no I/O.
func (d *DeployDetector) Append(e model.Entry) {
	if e.App == "" {
		return
	}
	version := versionFromEntry(e)
	if version == "" {
		return
	}
	ts := e.Ts
	if ts.IsZero() {
		ts = time.Now()
	}

	d.mu.Lock()
	defer d.mu.Unlock()
	k := nodeKey{e.TenantID, e.App}
	if d.last[k] == version {
		return
	}
	if _, seen := d.last[k]; !seen && len(d.last) >= maxTrackedApps {
		return
	}
	d.last[k] = version
	d.pending[e.TenantID] = append(d.pending[e.TenantID], Deploy{App: e.App, Version: version, Ts: ts})
}

// versionFromEntry extracts a version: explicit fields first, then message
// heuristics.
func versionFromEntry(e model.Entry) string {
	if v := firstField(e.Fields, versionFields); v != "" {
		return v
	}
	if m := deployMsgRe.FindStringSubmatch(e.Msg); m != nil {
		return m[1]
	}
	if m := versionMsgRe.FindStringSubmatch(e.Msg); m != nil {
		return m[1]
	}
	return ""
}

// Close flushes pending markers and stops the loop.
func (d *DeployDetector) Close() {
	close(d.stop)
	<-d.done
}

func (d *DeployDetector) loop() {
	defer close(d.done)
	ticker := time.NewTicker(5 * time.Second)
	defer ticker.Stop()
	for {
		select {
		case <-ticker.C:
			d.flush()
		case <-d.stop:
			d.flush()
			return
		}
	}
}

func (d *DeployDetector) flush() {
	d.mu.Lock()
	pending := d.pending
	d.pending = make(map[string][]Deploy)
	d.mu.Unlock()

	for tenantID, deploys := range pending {
		ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
		if err := d.store.InsertDeploys(ctx, tenantID, deploys); err != nil {
			slog.Warn("graph: deploy markers insert failed", "err", err)
		}
		cancel()
	}
}
