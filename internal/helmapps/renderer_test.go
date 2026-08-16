package helmapps

import (
	"errors"
	"strings"
	"testing"
)

func TestRenderPlanHasExactNetworkOffArgumentAndSandboxContract(t *testing.T) {
	files := testChartFiles()
	packageBytes := packageChart(t, files)
	approval := testApproval(t, packageBytes, files)
	bundle, err := InspectChartPackage(approval, packageBytes)
	if err != nil {
		t.Fatal(err)
	}
	plan, err := NewRenderPlan(approval, testDesired(t, approval, []byte("replicas: 2\n")), bundle)
	if err != nil {
		t.Fatal(err)
	}
	expected := []string{"template", "sample", RendererChartPath, "--namespace", "project-sample",
		"--values", RendererValuesPath, "--kube-version", RendererKubeVersion}
	if len(plan.Arguments) != len(expected) {
		t.Fatalf("arguments: %#v", plan.Arguments)
	}
	for index := range expected {
		if plan.Arguments[index] != expected[index] {
			t.Fatalf("argument %d: got %q want %q", index, plan.Arguments[index], expected[index])
		}
	}
	joined := strings.Join(plan.Arguments, " ")
	for _, forbidden := range []string{"credential", "kubeconfig", "post-renderer", "dependency-update", "include-crds", "skip-schema", "pass-credentials"} {
		if strings.Contains(joined, forbidden) {
			t.Fatalf("forbidden renderer option present: %s", forbidden)
		}
	}
	if plan.Sandbox != ExpectedSandboxContract() || plan.Sandbox.NetworkMode != "none" ||
		plan.Sandbox.AutomountServiceAccount || !plan.Sandbox.ReadOnlyRootFilesystem ||
		!plan.Sandbox.DropAllCapabilities || plan.Sandbox.AllowPrivilegeEscalation {
		t.Fatalf("unsafe sandbox: %+v", plan.Sandbox)
	}
}

func TestRenderedManifestAllowlistAndIdentityPolicy(t *testing.T) {
	files := testChartFiles()
	approval := testApproval(t, packageChart(t, files), files)
	descriptor, err := NewDescriptor(approval, testDestination())
	if err != nil {
		t.Fatal(err)
	}
	valid := validConfigMapManifest(descriptor)
	validated, err := ValidateRenderedManifests(valid, descriptor)
	if err != nil || validated.ResourceCount != 1 || !validDigest(validated.InventoryDigest) {
		t.Fatalf("valid namespaced manifest rejected: %+v %v", validated, err)
	}

	missingLabel := strings.Replace(string(valid), "    kuberploy.io/project: "+testProjectID+"\n", "", 1)
	hook := strings.Replace(string(valid), "  labels:\n", "  annotations:\n    helm.sh/hook: pre-install\n  labels:\n", 1)
	wrongNamespace := strings.Replace(string(valid), "namespace: project-sample", "namespace: kube-system", 1)
	secret := strings.Replace(string(valid), "kind: ConfigMap", "kind: Secret", 1)
	clusterRole := strings.Replace(strings.Replace(string(valid), "apiVersion: v1", "apiVersion: rbac.authorization.k8s.io/v1", 1), "kind: ConfigMap", "kind: ClusterRole", 1)
	crd := strings.Replace(strings.Replace(string(valid), "apiVersion: v1", "apiVersion: apiextensions.k8s.io/v1", 1), "kind: ConfigMap", "kind: CustomResourceDefinition", 1)
	status := string(valid) + "status: {}\n"
	duplicate := string(valid) + "---\n" + string(valid)
	service := strings.Replace(strings.Replace(string(valid), "kind: ConfigMap", "kind: Service", 1), "data:\n  hello: world", "spec:\n  type: LoadBalancer\n  ports:\n    - port: 80", 1)
	malformedService := strings.Replace(strings.Replace(string(valid), "kind: ConfigMap", "kind: Service", 1), "data:\n  hello: world", "spec:\n  ports: not-a-list", 1)
	serviceAccount := strings.Replace(strings.Replace(string(valid), "kind: ConfigMap", "kind: ServiceAccount", 1), "data:\n  hello: world", "automountServiceAccountToken: true", 1)
	secretWorkload := strings.Replace(strings.Replace(string(valid), "apiVersion: v1", "apiVersion: apps/v1", 1), "kind: ConfigMap", "kind: Deployment", 1)
	secretWorkload = strings.Replace(secretWorkload, "data:\n  hello: world", "spec:\n  template:\n    spec:\n      containers:\n        - name: app\n          image: example/app:latest\n          env:\n            - name: TOKEN\n              valueFrom:\n                secretKeyRef:\n                  name: other-app-secret\n                  key: token", 1)
	secretJob := strings.Replace(strings.Replace(string(valid), "apiVersion: v1", "apiVersion: batch/v1", 1), "kind: ConfigMap", "kind: Job", 1)
	secretJob = strings.Replace(secretJob, "data:\n  hello: world", "spec:\n  template:\n    spec:\n      restartPolicy: Never\n      containers:\n        - name: job\n          image: example/job:latest\n          envFrom:\n            - secretRef:\n                name: other-app-secret", 1)
	secretCronJob := strings.Replace(strings.Replace(string(valid), "apiVersion: v1", "apiVersion: batch/v1", 1), "kind: ConfigMap", "kind: CronJob", 1)
	secretCronJob = strings.Replace(secretCronJob, "data:\n  hello: world", "spec:\n  schedule: \"*/5 * * * *\"\n  jobTemplate:\n    spec:\n      template:\n        spec:\n          restartPolicy: Never\n          containers:\n            - name: cron\n              image: example/cron:latest\n              volumeMounts:\n                - name: credentials\n                  mountPath: /var/run/credentials\n          volumes:\n            - name: credentials\n              secret:\n                secretName: other-app-secret", 1)
	for name, raw := range map[string]string{
		"missing identity label": missingLabel, "hook": hook, "wrong namespace": wrongNamespace,
		"secret": secret, "cluster role": clusterRole, "crd": crd, "status": status,
		"duplicate identity": duplicate, "load balancer service": service,
		"malformed service ports": malformedService, "service account token": serviceAccount,
		"workload secret reference": secretWorkload,
		"job secret reference":      secretJob,
		"cronjob secret reference":  secretCronJob,
	} {
		t.Run(name, func(t *testing.T) {
			if _, err := ValidateRenderedManifests([]byte(raw), descriptor); !errors.Is(err, ErrUnsafeChart) {
				t.Fatalf("unsafe manifest accepted: %v", err)
			}
		})
	}
	safeCronJob := strings.Replace(
		secretCronJob,
		"          volumes:\n            - name: credentials\n              secret:\n                secretName: other-app-secret",
		"          volumes:\n            - name: credentials\n              emptyDir: {}",
		1,
	)
	if _, err := ValidateRenderedManifests([]byte(safeCronJob), descriptor); err != nil {
		t.Fatalf("safe CronJob rejected: %v", err)
	}
	safeWorkload := strings.Replace(
		secretWorkload,
		"              valueFrom:\n                secretKeyRef:\n                  name: other-app-secret\n                  key: token",
		"              value: ordinary-value\n      nodeSelector:\n        secret: \"true\"",
		1,
	)
	if _, err := ValidateRenderedManifests([]byte(safeWorkload), descriptor); err != nil {
		t.Fatalf("valid PodSpec label rejected: %v", err)
	}
}
