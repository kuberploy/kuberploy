package scheduling

import (
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

func TestRuntimeChartRendersClosedPreferredAndAntiAffinity(t *testing.T) {
	helm, err := exec.LookPath("helm")
	if err != nil {
		t.Skip("helm is not installed")
	}
	chart := filepath.Join("..", "..", "charts", "kuberploy-runtime")
	fixture := filepath.Join(chart, "testdata", "workload-scheduling.yaml")
	output, err := exec.Command(helm, "template", "scheduling", chart, "-f", fixture).CombinedOutput()
	if err != nil {
		t.Fatalf("helm template: %v\n%s", err, output)
	}
	rendered := string(output)
	for _, expected := range []string{
		"preferredDuringSchedulingIgnoredDuringExecution:",
		"weight: 75",
		"weight: 40",
		"podAntiAffinity:",
		"kuberploy.io/application: 33333333-3333-4333-8333-333333333333",
	} {
		if !strings.Contains(rendered, expected) {
			t.Fatalf("runtime chart omitted %q:\n%s", expected, output)
		}
	}
	for _, forbidden := range []string{"kind: Node\n", "kind: NodePool\n", "karpenter.sh/v1", "kind: ClusterRole\n", "kind: ClusterRoleBinding\n"} {
		if strings.Contains(rendered, forbidden) {
			t.Fatalf("runtime scheduling rendered forbidden cluster authority %q", forbidden)
		}
	}
}

func TestRuntimeChartRejectsSchemaBypassedAntiAffinityInjection(t *testing.T) {
	helm, err := exec.LookPath("helm")
	if err != nil {
		t.Skip("helm is not installed")
	}
	chart := filepath.Join("..", "..", "charts", "kuberploy-runtime")
	fixture := filepath.Join(chart, "testdata", "workload-scheduling.yaml")
	raw, err := os.ReadFile(fixture)
	if err != nil {
		t.Fatal(err)
	}
	applicationID := "33333333-3333-4333-8333-333333333333"
	cases := map[string]string{
		"cross-application selector": strings.Replace(string(raw), "kuberploy.io/application: "+applicationID, "kuberploy.io/application: 66666666-6666-4666-8666-666666666666", 1),
		"arbitrary selector":         strings.Replace(string(raw), "kuberploy.io/application: "+applicationID, "attacker.example/target: other", 1),
		"selector expression":        strings.Replace(string(raw), "              matchLabels:\n                kuberploy.io/application: "+applicationID, "              matchLabels:\n                kuberploy.io/application: "+applicationID+"\n              matchExpressions:\n                - key: attacker.example/target\n                  operator: Exists", 1),
		"namespace selector":         strings.Replace(string(raw), "          - topologyKey: kubernetes.io/hostname", "          - topologyKey: kubernetes.io/hostname\n            namespaceSelector: {}", 1),
		"mismatch label keys":        strings.Replace(string(raw), "          - topologyKey: kubernetes.io/hostname", "          - topologyKey: kubernetes.io/hostname\n            mismatchLabelKeys: [kuberploy.io/application]", 1),
		"unbounded weight":           strings.Replace(string(raw), "          - weight: 40", "          - weight: 101", 1),
		"raw pod affinity":           strings.Replace(string(raw), "      podAntiAffinity:", "      podAffinity:\n        requiredDuringSchedulingIgnoredDuringExecution:\n          - topologyKey: kubernetes.io/hostname\n            labelSelector:\n              matchLabels:\n                attacker.example/target: other\n      podAntiAffinity:", 1),
	}
	for name, values := range cases {
		t.Run(name, func(t *testing.T) {
			path := filepath.Join(t.TempDir(), "invalid.yaml")
			if err := os.WriteFile(path, []byte(values), 0o600); err != nil {
				t.Fatal(err)
			}
			if output, commandErr := exec.Command(helm, "template", "invalid", chart, "-f", path, "--skip-schema-validation").CombinedOutput(); commandErr == nil {
				t.Fatalf("runtime template accepted schema-bypassed %s:\n%s", name, output)
			}
		})
	}
}
