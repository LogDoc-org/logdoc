package graph

import (
	"errors"
	"io"
	"strings"
	"testing"

	"gopkg.in/yaml.v3"
)

func decodeBackstage(t *testing.T, out string) map[string]bsEntity {
	t.Helper()
	ents := map[string]bsEntity{}
	dec := yaml.NewDecoder(strings.NewReader(out))
	for {
		var e bsEntity
		if err := dec.Decode(&e); err != nil {
			if errors.Is(err, io.EOF) {
				break
			}
			t.Fatalf("decode: %v", err)
		}
		ents[e.Metadata.Name] = e
	}
	return ents
}

func TestBackstageExport(t *testing.T) {
	topo := Topology{
		Nodes: []Node{{App: "api"}, {App: "billing"}, {App: "весы/кг"}},
		Edges: []Edge{
			{Src: "api", Dst: "billing"},
			{Src: "api", Dst: "billing"}, // duplicate edge must not duplicate dependsOn
			{Src: "billing", Dst: "весы/кг"},
		},
	}
	metas := []ServiceMeta{
		{
			App: "billing", Owner: "Team Payments", Description: "charges cards",
			Links: map[string]string{"repo": "https://github.com/x/y", "runbook": "https://wiki/rb"},
			Tags:  []string{"Critical", "PCI"},
		},
		// Declared but never logged: still a component.
		{App: "ledger", Owner: "team-fin"},
	}

	ents := decodeBackstage(t, Backstage(topo, metas))
	if len(ents) != 4 {
		t.Fatalf("want 4 components, got %d: %v", len(ents), ents)
	}

	b := ents["billing"]
	if b.APIVersion != "backstage.io/v1alpha1" || b.Kind != "Component" {
		t.Errorf("entity header: %+v", b)
	}
	if b.Spec.Owner != "team-payments" || b.Spec.Type != "service" {
		t.Errorf("spec: %+v", b.Spec)
	}
	if b.Metadata.Description != "charges cards" {
		t.Errorf("description: %+v", b.Metadata)
	}
	if len(b.Metadata.Links) != 2 || b.Metadata.Links[0].Title != "repo" {
		t.Errorf("links: %+v", b.Metadata.Links)
	}
	if len(b.Metadata.Tags) != 2 || b.Metadata.Tags[0] != "critical" || b.Metadata.Tags[1] != "pci" {
		t.Errorf("tags: %+v", b.Metadata.Tags)
	}

	api := ents["api"]
	if api.Spec.Owner != "unknown" {
		t.Errorf("ownerless service: %+v", api.Spec)
	}
	if len(api.Spec.DependsOn) != 1 || api.Spec.DependsOn[0] != "component:billing" {
		t.Errorf("dependsOn must be deduplicated: %+v", api.Spec.DependsOn)
	}

	if _, ok := ents["ledger"]; !ok {
		t.Errorf("declared-only service missing: %v", ents)
	}

	// The unicode app gets a sanitized unique name, keeps the original as
	// title, and the edge to it survives.
	var scales *bsEntity
	for name, e := range ents {
		if e.Metadata.Title == "весы/кг" {
			ee := e
			scales = &ee
			if strings.Trim(name, "abcdefghijklmnopqrstuvwxyzABCDEFGHIJKLMNOPQRSTUVWXYZ0123456789-_.") != "" {
				t.Errorf("sanitized name %q has invalid characters", name)
			}
		}
	}
	if scales == nil {
		t.Fatalf("unicode app missing: %v", ents)
	}
	if ents["billing"].Spec.DependsOn[0] != "component:"+scales.Metadata.Name {
		t.Errorf("edge to sanitized name: %+v", ents["billing"].Spec.DependsOn)
	}
	if ents["billing"].Metadata.Annotations["logdoc.org/app"] != "billing" {
		t.Errorf("annotations: %+v", ents["billing"].Metadata.Annotations)
	}
}

func TestBackstageNameCollisions(t *testing.T) {
	names := backstageNames([]string{"a/b", "a b", "a?b"})
	seen := map[string]bool{}
	for app, name := range names {
		if seen[name] {
			t.Errorf("collision on %q for %q: %v", name, app, names)
		}
		seen[name] = true
	}
}
