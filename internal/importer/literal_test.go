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

func TestCleanYAMLLiteralBlocks(t *testing.T) {
	cases := map[string]string{
		"trailing-space": blackboxRules,
		"tab":            "a:\tb\nc\n",
		"no-final-nl":    "x \ny",
		"keep-trailing":  "x \n\n\n",
		"blank-lines":    "x \n\n  y\n",
		"crlf":           "a\r\nb\n",
		"leading-space":  "  a \nb\n",
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
		"list":     []any{map[string]any{"k": blackboxRules}, blackboxRules},
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
	if got.List[1] != blackboxRules || got.List[0].(map[string]any)["k"] != blackboxRules {
		t.Errorf("list values changed:\n%s", out)
	}
	for _, want := range []string{
		"  trailing-space: |\n    groups:\n      - name: blackbox\n",
		"  tab: |\n    a:\tb\n",
		"  no-final-nl: |-\n",
		"  keep-trailing: |+\n",
		"  plain: |\n",
		"  crlf: \"a\\r\\nb\\n\"\n",
		"- k: |\n    groups:\n",
		"- |\n    groups:\n",
	} {
		if !strings.Contains(out, want) {
			t.Errorf("missing %q in:\n%s", want, out)
		}
	}
}
