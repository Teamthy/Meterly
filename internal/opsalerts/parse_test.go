package opsalerts

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"gopkg.in/yaml.v3"
)

func TestAlertsYAMLHasRunbooks(t *testing.T) {
	root := filepath.Join("..", "..")
	raw, err := os.ReadFile(filepath.Join(root, "deployments", "monitoring", "alerts.yml"))
	if err != nil {
		t.Fatal(err)
	}
	var doc struct {
		Groups []struct {
			Name  string `yaml:"name"`
			Rules []struct {
				Alert       string            `yaml:"alert"`
				Expr        string            `yaml:"expr"`
				For         string            `yaml:"for"`
				Labels      map[string]string `yaml:"labels"`
				Annotations map[string]string `yaml:"annotations"`
			} `yaml:"rules"`
		} `yaml:"groups"`
	}
	if err := yaml.Unmarshal(raw, &doc); err != nil {
		t.Fatalf("parse alerts.yml: %v", err)
	}
	if len(doc.Groups) == 0 {
		t.Fatal("no groups")
	}
	seen := map[string]bool{}
	for _, g := range doc.Groups {
		for _, r := range g.Rules {
			if r.Alert == "" || r.Expr == "" {
				t.Fatalf("rule missing alert or expr: %+v", r)
			}
			if seen[r.Alert] {
				t.Fatalf("duplicate alert %q", r.Alert)
			}
			seen[r.Alert] = true
			rb := r.Annotations["runbook"]
			if rb == "" || !strings.HasPrefix(rb, "docs/runbooks/") || !strings.HasSuffix(rb, ".md") {
				t.Fatalf("%s missing docs/runbooks/*.md annotation, got %q", r.Alert, rb)
			}
			if _, err := os.Stat(filepath.Join(root, filepath.FromSlash(rb))); err != nil {
				t.Fatalf("%s runbook %s: %v", r.Alert, rb, err)
			}
			if r.Labels["severity"] == "" {
				t.Fatalf("%s missing severity", r.Alert)
			}
		}
	}
	for _, want := range []string{"VendingPaused", "WorkerHeartbeatStale", "OutboxLag", "HTTP5xx"} {
		if !seen[want] {
			t.Fatalf("missing required alert %s", want)
		}
	}
}
