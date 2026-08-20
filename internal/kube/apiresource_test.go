package kube

import (
	"errors"
	"testing"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/kubernetes/fake"
)

func testDiscovery() *fake.Clientset {
	cs := fake.NewClientset()
	cs.Resources = []*metav1.APIResourceList{
		{
			GroupVersion: "v1",
			APIResources: []metav1.APIResource{
				{Name: "pods", SingularName: "pod", Kind: "Pod", Namespaced: true, ShortNames: []string{"po"}},
				{Name: "pods/status", Kind: "Pod", Namespaced: true},
				{Name: "nodes", SingularName: "node", Kind: "Node", Namespaced: false, ShortNames: []string{"no"}},
			},
		},
		{
			GroupVersion: "apps/v1",
			APIResources: []metav1.APIResource{
				{Name: "deployments", SingularName: "deployment", Kind: "Deployment", Namespaced: true, ShortNames: []string{"deploy"}},
			},
		},
	}
	return cs
}

func TestGroupVersion(t *testing.T) {
	if got := GroupVersion("", "v1"); got != "v1" {
		t.Errorf("core GroupVersion = %q, want v1", got)
	}
	if got := GroupVersion("apps", "v1"); got != "apps/v1" {
		t.Errorf("GroupVersion = %q, want apps/v1", got)
	}
}

func TestNormalizeGroup(t *testing.T) {
	if got := NormalizeGroup(CoreGroupSentinel); got != "" {
		t.Errorf("NormalizeGroup(%q) = %q, want empty", CoreGroupSentinel, got)
	}
	if got := NormalizeGroup("apps"); got != "apps" {
		t.Errorf("NormalizeGroup(apps) = %q", got)
	}
	if got := NormalizeGroup(""); got != "" {
		t.Errorf("NormalizeGroup(\"\") = %q", got)
	}
}

func TestResolveAPIResourceSpellings(t *testing.T) {
	d := testDiscovery().Discovery()
	for _, token := range []string{"Deployment", "deployment", "deployments", "deploy", "DEPLOY"} {
		res, err := ResolveAPIResource(d, "apps", "v1", token)
		if err != nil {
			t.Fatalf("%q: %v", token, err)
		}
		if res.Kind != "Deployment" || res.Name != "deployments" {
			t.Errorf("%q resolved to %s/%s", token, res.Kind, res.Name)
		}
		// The per-group-version list omits group and version; a caller building
		// a GVR from the result depends on them being filled back in.
		if res.Group != "apps" || res.Version != "v1" {
			t.Errorf("%q: coordinates not populated: %+v", token, res)
		}
	}
}

// A subresource is an operation on a Kind, not a Kind — resolving to it would
// build a GVR that fetches nothing.
func TestResolveAPIResourceSkipsSubresources(t *testing.T) {
	res, err := ResolveAPIResource(testDiscovery().Discovery(), "", "v1", "Pod")
	if err != nil {
		t.Fatalf("ResolveAPIResource: %v", err)
	}
	if res.Name != "pods" {
		t.Errorf("Pod resolved to %q, want pods", res.Name)
	}
}

func TestResolveAPIResourceErrors(t *testing.T) {
	d := testDiscovery().Discovery()
	if _, err := ResolveAPIResource(d, "bogus.io", "v1", "Thing"); !errors.Is(err, ErrUnknownGroupVersion) {
		t.Errorf("expected ErrUnknownGroupVersion, got %v", err)
	}
	if _, err := ResolveAPIResource(d, "apps", "v1", "Nope"); !errors.Is(err, ErrUnknownKind) {
		t.Errorf("expected ErrUnknownKind, got %v", err)
	}
	// Scope travels with the answer: the caller validates namespace against it.
	res, err := ResolveAPIResource(d, "", "v1", "no")
	if err != nil {
		t.Fatalf("ResolveAPIResource(no): %v", err)
	}
	if res.Kind != "Node" || res.Namespaced {
		t.Errorf("short name \"no\" resolved to %+v", res)
	}
}
