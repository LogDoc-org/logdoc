package graph

import (
	"context"
	"sync"
	"testing"
	"time"

	"github.com/LogDoc-org/logdoc/internal/model"
)

func TestVersionFromEntry(t *testing.T) {
	cases := []struct {
		name string
		e    model.Entry
		want string
	}{
		{"field version", model.Entry{Fields: map[string]string{"version": "2.3.1"}}, "2.3.1"},
		{"field app_version", model.Entry{Fields: map[string]string{"app_version": "v1.0.0-rc1"}}, "v1.0.0-rc1"},
		{"field service.version", model.Entry{Fields: map[string]string{"service.version": "3.2"}}, "3.2"},
		{"msg deploy", model.Entry{Msg: "deploy billing 2.3.1: pool 100"}, "2.3.1"},
		{"msg deployed v-prefix", model.Entry{Msg: "deployed v10.2.0 to production"}, "10.2.0"},
		{"msg rolled out", model.Entry{Msg: "rolled out 1.4.7-hotfix"}, "1.4.7-hotfix"},
		{"msg version colon", model.Entry{Msg: "starting server, version: 5.1.2"}, "5.1.2"},
		{"msg version equals", model.Entry{Msg: "boot ok version=v2.0.0"}, "2.0.0"},
		{"no false positive on duration", model.Entry{Msg: "started in 1.2 seconds"}, ""},
		{"no false positive plain", model.Entry{Msg: "charge failed: timeout after 2.5s"}, ""},
		{"no version at all", model.Entry{Msg: "hello world"}, ""},
	}
	for _, c := range cases {
		if got := versionFromEntry(c.e); got != c.want {
			t.Errorf("%s: got %q want %q", c.name, got, c.want)
		}
	}
}

type fakeDeployStore struct {
	mu       sync.Mutex
	inserted []Deploy
}

func (f *fakeDeployStore) InsertDeploys(_ context.Context, _ string, ds []Deploy) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.inserted = append(f.inserted, ds...)
	return nil
}

func (f *fakeDeployStore) Deploys(context.Context, string, string, time.Time, int) ([]Deploy, error) {
	return nil, nil
}

func (f *fakeDeployStore) count() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return len(f.inserted)
}

func TestDeployDetector(t *testing.T) {
	store := &fakeDeployStore{}
	d := NewDeployDetector(store)

	ts := time.Now()
	entry := func(app, version string) model.Entry {
		return model.Entry{TenantID: model.DefaultTenant, App: app, Ts: ts,
			Fields: map[string]string{"version": version}}
	}

	d.Append(entry("billing", "2.3.0"))
	d.Append(entry("billing", "2.3.0")) // same version — no new marker
	d.Append(entry("billing", "2.3.1")) // change — marker
	d.Append(entry("api", "1.0.0"))
	d.Append(model.Entry{TenantID: model.DefaultTenant, App: "web", Ts: ts, Msg: "no version here"})

	d.Close() // flushes

	if got := store.count(); got != 3 {
		t.Fatalf("markers: got %d want 3 (%+v)", got, store.inserted)
	}
	versions := map[string]string{}
	for _, dep := range store.inserted {
		versions[dep.App+"@"+dep.Version] = dep.Version
	}
	for _, want := range []string{"billing@2.3.0", "billing@2.3.1", "api@1.0.0"} {
		if _, ok := versions[want]; !ok {
			t.Errorf("missing marker %s in %+v", want, store.inserted)
		}
	}
}
