package importer

import (
	"strings"
	"testing"

	"sigs.k8s.io/yaml"
)

// Prometheus rules with a trailing space made the YAML library fall back to
// a double-quoted, line-folded string.
const blackboxRules = `groups:
  - name: blackbox
    rules:
      - alert: ssl_cert_expires_in_30_days
        expr: probe_ssl_earliest_cert_expiry{job="blackbox-exporter"} - time() < 86400 * 30 
        for: 1m
        annotations:
          description: SSL Cert {{ $labels.instance }} will expire
`

// Indented JSON from a Swarm config: starts with a blank line and has a
// whitespace-only line before the end.
const graphSettings = "\n    {\n        \"GRAPHAPIAPPID\": \"128e4b1c\",\n        \"GRAPHMAILADDRESS\": \"foo@example.com\"\n    }\n    \n"

func TestCleanYAMLLiteralBlocks(t *testing.T) {
	cases := map[string]string{
		"trailing-space": blackboxRules,
		"tab":            "a:\tb\nc\n",
		"no-final-nl":    "x \ny",
		"keep-trailing":  "x \n\n\n",
		"blank-lines":    "x \n\n  y\n",
		"crlf":           "a\r\nb\n",
		"leading-space":  "  a \nb\n",
		"json":           graphSettings,
		"ws-first":       "  \n\tx\n",
		"lead-tab":       "\ta\nb",
		"plain":          "a: 1\nb: 2\n",
		"single":         "one line ",
	}
	data := map[string]string{}
	for k, v := range cases {
		data[k] = v
	}
	cm := map[string]any{
		"apiVersion": "v1", "kind": "ConfigMap",
		"metadata": map[string]any{"name": "rules"},
		"data":     data,
		"list":     []any{map[string]any{"k": blackboxRules}, blackboxRules, graphSettings, map[string]any{"j": graphSettings}},
	}
	b, _ := yaml.Marshal(cm)
	out := string(cleanYAML(b))

	var got struct {
		Data map[string]string `json:"data"`
		List []any             `json:"list"`
	}
	if err := yaml.Unmarshal([]byte(out), &got); err != nil {
		t.Fatalf("unmarshal: %v\n%s", err, out)
	}
	for k, v := range cases {
		if got.Data[k] != v {
			t.Errorf("%s: round trip got %q, want %q", k, got.Data[k], v)
		}
	}
	if got.List[2] != graphSettings || got.List[3].(map[string]any)["j"] != graphSettings {
		t.Errorf("indented list values changed:\n%s", out)
	}
	if got.List[1] != blackboxRules || got.List[0].(map[string]any)["k"] != blackboxRules {
		t.Errorf("list values changed:\n%s", out)
	}
	for _, want := range []string{
		"  trailing-space: |\n    groups:\n      - name: blackbox\n",
		"  tab: |\n    a:\tb\n",
		"  no-final-nl: |-\n",
		"  keep-trailing: |+\n",
		"  plain: |\n",
		"  leading-space: |2\n      a \n    b\n",
		"  json: |2\n\n        {\n            \"GRAPHAPIAPPID\": ",
		"  ws-first: |2\n",
		"  lead-tab: |2-\n",
		"- |4\n\n        {\n",
		"- j: |2\n\n        {\n",
		"  crlf: \"a\\r\\nb\\n\"\n",
		"- k: |\n    groups:\n",
		"- |\n    groups:\n",
	} {
		if !strings.Contains(out, want) {
			t.Errorf("missing %q in:\n%s", want, out)
		}
	}
}
