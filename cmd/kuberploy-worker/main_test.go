package main

import "testing"

func TestRunRejectsProviderKubeAPIOverlapBeforeDependencies(t *testing.T) {
	t.Setenv("KUBERPLOY_EXTERNAL_EGRESS_CIDRS", "10.43.0.0/16")
	t.Setenv("KUBERPLOY_KUBE_API_SERVER_CIDRS", "10.43.0.1/32")
	if err := run(); err == nil {
		t.Fatal("worker accepted overlapping provider and Kubernetes API CIDRs")
	}
}
