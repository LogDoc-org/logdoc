package graph

import (
	"fmt"
	"sort"
	"strings"

	"gopkg.in/yaml.v3"
)

// Backstage renders the topology plus catalog metadata as Backstage catalog
// entities (multi-document YAML, one kind: Component per service) — the
// format catalog-info.yaml files use. Point Backstage's URL reader at the
// export endpoint and its catalog keeps itself fresh from logs: dependsOn
// comes from the observed edges, owner/description/links/tags from the
// service catalog.
func Backstage(t Topology, metas []ServiceMeta) string {
	metaByApp := make(map[string]ServiceMeta, len(metas))
	for _, m := range metas {
		metaByApp[m.App] = m
	}

	// A component per observed service, plus declared-only services: an
	// entry in the catalog is a service even before its first log line.
	apps := make([]string, 0, len(t.Nodes)+len(metas))
	seen := make(map[string]bool, len(t.Nodes))
	for _, n := range t.Nodes {
		apps = append(apps, n.App)
		seen[n.App] = true
	}
	for _, m := range metas {
		if !seen[m.App] {
			apps = append(apps, m.App)
		}
	}
	sort.Strings(apps)

	names := backstageNames(apps)
	dependsOn := make(map[string][]string)
	for _, e := range t.Edges {
		src, sok := names[e.Src]
		dst, dok := names[e.Dst]
		if !sok || !dok {
			continue
		}
		dependsOn[src] = append(dependsOn[src], "component:"+dst)
	}

	var b strings.Builder
	for _, app := range apps {
		meta := metaByApp[app]
		name := names[app]

		ent := bsEntity{
			APIVersion: "backstage.io/v1alpha1",
			Kind:       "Component",
			Metadata: bsMetadata{
				Name:        name,
				Description: meta.Description,
				Tags:        backstageTags(meta.Tags),
				Annotations: map[string]string{"logdoc.org/app": app},
			},
			Spec: bsSpec{
				Type:      "service",
				Lifecycle: "unknown",
				Owner:     backstageOwner(meta.Owner),
				DependsOn: sortedUnique(dependsOn[name]),
			},
		}
		if name != app {
			ent.Metadata.Title = app
		}
		linkNames := make([]string, 0, len(meta.Links))
		for n := range meta.Links {
			linkNames = append(linkNames, n)
		}
		sort.Strings(linkNames)
		for _, n := range linkNames {
			ent.Metadata.Links = append(ent.Metadata.Links, bsLink{URL: meta.Links[n], Title: n})
		}

		out, err := yaml.Marshal(ent)
		if err != nil {
			continue // a single unmarshalable entity must not kill the export
		}
		b.WriteString("---\n")
		b.Write(out)
	}
	return b.String()
}

type bsEntity struct {
	APIVersion string     `yaml:"apiVersion"`
	Kind       string     `yaml:"kind"`
	Metadata   bsMetadata `yaml:"metadata"`
	Spec       bsSpec     `yaml:"spec"`
}

type bsMetadata struct {
	Name        string            `yaml:"name"`
	Title       string            `yaml:"title,omitempty"` // the original app name when sanitized
	Description string            `yaml:"description,omitempty"`
	Annotations map[string]string `yaml:"annotations,omitempty"`
	Tags        []string          `yaml:"tags,omitempty"`
	Links       []bsLink          `yaml:"links,omitempty"`
}

type bsLink struct {
	URL   string `yaml:"url"`
	Title string `yaml:"title,omitempty"`
}

type bsSpec struct {
	Type      string   `yaml:"type"`
	Lifecycle string   `yaml:"lifecycle"`
	Owner     string   `yaml:"owner"`
	DependsOn []string `yaml:"dependsOn,omitempty"`
}

// backstageNames maps app names to valid, unique Backstage entity names:
// [a-zA-Z0-9] plus -_. separators, at most 63 characters.
func backstageNames(apps []string) map[string]string {
	names := make(map[string]string, len(apps))
	taken := make(map[string]bool, len(apps))
	for _, app := range apps {
		name := sanitizeBackstageName(app)
		for i := 2; taken[name]; i++ {
			name = fmt.Sprintf("%s-%d", truncate(sanitizeBackstageName(app), 59), i)
		}
		names[app] = name
		taken[name] = true
	}
	return names
}

func sanitizeBackstageName(app string) string {
	if name := backstageAlphabet(app); name != "" {
		return name
	}
	return "service"
}

// backstageAlphabet keeps [a-zA-Z0-9-_.], replaces the rest with '-';
// empty when nothing survives.
func backstageAlphabet(s string) string {
	var b strings.Builder
	for _, r := range s {
		switch {
		case r >= 'a' && r <= 'z', r >= 'A' && r <= 'Z', r >= '0' && r <= '9',
			r == '-', r == '_', r == '.':
			b.WriteRune(r)
		default:
			b.WriteByte('-')
		}
	}
	return truncate(strings.Trim(b.String(), "-_."), 63)
}

// backstageTags lowercases and sanitizes tags to Backstage's alphabet
// ([a-z0-9:+#-]); unusable tags are dropped.
func backstageTags(tags []string) []string {
	var out []string
	for _, t := range tags {
		var b strings.Builder
		for _, r := range strings.ToLower(t) {
			switch {
			case r >= 'a' && r <= 'z', r >= '0' && r <= '9',
				r == ':', r == '+', r == '#', r == '-':
				b.WriteRune(r)
			default:
				b.WriteByte('-')
			}
		}
		tag := truncate(strings.Trim(b.String(), "-"), 63)
		if tag != "" {
			out = append(out, tag)
		}
	}
	return out
}

// backstageOwner turns the free-form owner field into an entity reference;
// Backstage requires spec.owner, so no usable owner becomes "unknown".
func backstageOwner(owner string) string {
	if name := backstageAlphabet(strings.ToLower(owner)); name != "" {
		return name
	}
	return "unknown"
}

func sortedUnique(in []string) []string {
	if len(in) == 0 {
		return nil
	}
	sort.Strings(in)
	out := in[:1]
	for _, s := range in[1:] {
		if s != out[len(out)-1] {
			out = append(out, s)
		}
	}
	return out
}

func truncate(s string, n int) string {
	if len(s) <= n {
		return s
	}
	return s[:n]
}
