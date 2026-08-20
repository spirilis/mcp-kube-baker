package tools

import (
	"strings"
	"testing"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/client-go/kubernetes/fake"
)

// unstructuredObj builds one arbitrary manifest the dynamic fake will serve.
func unstructuredObj(apiVersion, kind, namespace, name string, spec map[string]interface{}) *unstructured.Unstructured {
	obj := map[string]interface{}{
		"apiVersion": apiVersion,
		"kind":       kind,
		"metadata":   map[string]interface{}{"name": name},
	}
	if namespace != "" {
		obj["metadata"].(map[string]interface{})["namespace"] = namespace
	}
	if spec != nil {
		obj["spec"] = spec
	}
	return &unstructured.Unstructured{Object: obj}
}

// withCRD teaches the "test" context's discovery about a custom resource, the
// case this tool exists for: a Kind no compiled-in Go type covers.
func withCRD(c *fakeClients) *fakeClients {
	cs := c.sets["test"].(*fake.Clientset)
	cs.Resources = append(cs.Resources, &metav1.APIResourceList{
		GroupVersion: "cert-manager.io/v1",
		APIResources: []metav1.APIResource{
			{Name: "certificates", SingularName: "certificate", Kind: "Certificate", Namespaced: true, ShortNames: []string{"cert"}, Verbs: []string{"get", "list"}},
			{Name: "clusterissuers", SingularName: "clusterissuer", Kind: "ClusterIssuer", Namespaced: false, Verbs: []string{"get", "list"}},
		},
	})
	return c
}

func TestGetArbitraryManifestNamespaced(t *testing.T) {
	c := newFakeClients().withDynamic(
		unstructuredObj("apps/v1", "Deployment", "default", "web", map[string]interface{}{"replicas": int64(3)}),
	)
	res := callTool(t, NewGetArbitraryManifestHandler(c),
		`{"context":"test","api_group":"apps","api_version":"v1","kind":"Deployment","namespace":"default","name":"web"}`)
	if res.IsError {
		t.Fatalf("unexpected tool error: %+v", res.Content)
	}
	out := res.StructuredContent.(arbitraryManifestOutput)
	if out.Resource.Kind != "Deployment" || out.Resource.Resource != "deployments" || !out.Resource.Namespaced {
		t.Errorf("unexpected resolved resource: %+v", out.Resource)
	}
	if out.Resource.APIVersion != "apps/v1" {
		t.Errorf("api_version = %q", out.Resource.APIVersion)
	}
	if got, _, _ := unstructured.NestedInt64(out.Manifest, "spec", "replicas"); got != 3 {
		t.Errorf("manifest spec.replicas = %d, want 3", got)
	}
}

// Server-side apply bookkeeping is a third of a typical manifest and says
// nothing about the object; kubectl hides it by default too.
func TestGetArbitraryManifestDropsManagedFields(t *testing.T) {
	obj := unstructuredObj("apps/v1", "Deployment", "default", "web", nil)
	obj.SetManagedFields([]metav1.ManagedFieldsEntry{{Manager: "kubectl", Operation: metav1.ManagedFieldsOperationApply}})
	c := newFakeClients().withDynamic(obj)

	res := callTool(t, NewGetArbitraryManifestHandler(c),
		`{"context":"test","api_version":"apps/v1","kind":"Deployment","namespace":"default","name":"web"}`)
	if res.IsError {
		t.Fatalf("unexpected tool error: %+v", res.Content)
	}
	out := res.StructuredContent.(arbitraryManifestOutput)
	if _, found, _ := unstructured.NestedSlice(out.Manifest, "metadata", "managedFields"); found {
		t.Error("managedFields survived into the tool result")
	}
	if strings.Contains(contentText(t, res), "managedFields") {
		t.Error("managedFields survived into the text content")
	}
	// The rest of metadata is untouched.
	if name, _, _ := unstructured.NestedString(out.Manifest, "metadata", "name"); name != "web" {
		t.Errorf("metadata.name = %q after stripping", name)
	}
}

// The plural, singular and short spellings all resolve, because a model
// composing arguments from a kubectl-shaped mental model will reach for them.
func TestGetArbitraryManifestKindSpellings(t *testing.T) {
	c := newFakeClients().withDynamic(
		unstructuredObj("apps/v1", "Deployment", "default", "web", nil),
	)
	for _, kind := range []string{"Deployment", "deployment", "deployments", "deploy", "DEPLOYMENT"} {
		res := callTool(t, NewGetArbitraryManifestHandler(c),
			`{"context":"test","api_group":"apps","api_version":"v1","kind":"`+kind+`","namespace":"default","name":"web"}`)
		if res.IsError {
			t.Errorf("kind %q: unexpected tool error: %+v", kind, res.Content)
			continue
		}
		if out := res.StructuredContent.(arbitraryManifestOutput); out.Resource.Kind != "Deployment" {
			t.Errorf("kind %q resolved to %q", kind, out.Resource.Kind)
		}
	}
}

// "apps/v1" in api_version is the spelling that appears in every manifest a
// model has ever read; it must work with api_group left out.
func TestGetArbitraryManifestQualifiedAPIVersion(t *testing.T) {
	c := newFakeClients().withDynamic(
		unstructuredObj("apps/v1", "Deployment", "default", "web", nil),
	)
	res := callTool(t, NewGetArbitraryManifestHandler(c),
		`{"context":"test","api_version":"apps/v1","kind":"Deployment","namespace":"default","name":"web"}`)
	if res.IsError {
		t.Fatalf("unexpected tool error: %+v", res.Content)
	}
	out := res.StructuredContent.(arbitraryManifestOutput)
	if out.Resource.Group != "apps" || out.Resource.Version != "v1" {
		t.Errorf("unexpected group/version: %+v", out.Resource)
	}

	res = callTool(t, NewGetArbitraryManifestHandler(c),
		`{"context":"test","api_group":"batch","api_version":"apps/v1","kind":"Deployment","namespace":"default","name":"web"}`)
	if !res.IsError || !strings.Contains(contentText(t, res), "contradicts") {
		t.Errorf("expected a contradiction error, got %+v", res.Content)
	}
}

func TestGetArbitraryManifestClusterScoped(t *testing.T) {
	c := newFakeClients().withDynamic(
		unstructuredObj("v1", "Node", "", "node1", nil),
	)
	// The core group is spelled either way: omitted, or as the sentinel.
	for _, group := range []string{``, `"api_group":"core",`} {
		res := callTool(t, NewGetArbitraryManifestHandler(c),
			`{"context":"test",`+group+`"api_version":"v1","kind":"Node","name":"node1"}`)
		if res.IsError {
			t.Fatalf("group %q: unexpected tool error: %+v", group, res.Content)
		}
		out := res.StructuredContent.(arbitraryManifestOutput)
		if out.Resource.Namespaced || out.Resource.Group != "" || out.Resource.APIVersion != "v1" {
			t.Errorf("unexpected resolved resource: %+v", out.Resource)
		}
	}
}

func TestGetArbitraryManifestCustomResource(t *testing.T) {
	c := withCRD(newFakeClients()).withDynamic(
		unstructuredObj("cert-manager.io/v1", "Certificate", "istio-system", "gateway-cert",
			map[string]interface{}{"dnsNames": []interface{}{"example.com"}}),
		unstructuredObj("cert-manager.io/v1", "ClusterIssuer", "", "letsencrypt", nil),
	)
	res := callTool(t, NewGetArbitraryManifestHandler(c),
		`{"context":"test","api_group":"cert-manager.io","api_version":"v1","kind":"cert","namespace":"istio-system","name":"gateway-cert"}`)
	if res.IsError {
		t.Fatalf("unexpected tool error: %+v", res.Content)
	}
	out := res.StructuredContent.(arbitraryManifestOutput)
	if out.Resource.Kind != "Certificate" || out.Resource.Resource != "certificates" {
		t.Errorf("unexpected resolved resource: %+v", out.Resource)
	}
	names, _, _ := unstructured.NestedStringSlice(out.Manifest, "spec", "dnsNames")
	if len(names) != 1 || names[0] != "example.com" {
		t.Errorf("unexpected manifest content: %v", names)
	}

	res = callTool(t, NewGetArbitraryManifestHandler(c),
		`{"context":"test","api_group":"cert-manager.io","api_version":"v1","kind":"ClusterIssuer","name":"letsencrypt"}`)
	if res.IsError {
		t.Fatalf("unexpected tool error on cluster-scoped custom resource: %+v", res.Content)
	}
}

func TestGetArbitraryManifestScopeErrors(t *testing.T) {
	c := newFakeClients().withDynamic(
		unstructuredObj("apps/v1", "Deployment", "default", "web", nil),
		unstructuredObj("v1", "Node", "", "node1", nil),
	)
	cases := []struct {
		name, args, want string
	}{
		{
			"namespaced kind without namespace",
			`{"context":"test","api_group":"apps","api_version":"v1","kind":"Deployment","name":"web"}`,
			"is namespaced",
		},
		{
			"cluster-scoped kind with namespace",
			`{"context":"test","api_version":"v1","kind":"Node","namespace":"default","name":"node1"}`,
			"is cluster-scoped",
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			res := callTool(t, NewGetArbitraryManifestHandler(c), tc.args)
			if !res.IsError {
				t.Fatalf("expected a tool error, got %+v", res.StructuredContent)
			}
			if !strings.Contains(contentText(t, res), tc.want) {
				t.Errorf("error did not mention %q: %s", tc.want, contentText(t, res))
			}
		})
	}
}

func TestGetArbitraryManifestLookupErrors(t *testing.T) {
	c := newFakeClients().withDynamic(
		unstructuredObj("apps/v1", "Deployment", "default", "web", nil),
	)
	cases := []struct {
		name, args, want string
	}{
		{
			"unserved group-version",
			`{"context":"test","api_group":"bogus.io","api_version":"v1","kind":"Thing","namespace":"default","name":"x"}`,
			"does not serve api group-version \"bogus.io/v1\"",
		},
		{
			"unknown kind",
			`{"context":"test","api_group":"apps","api_version":"v1","kind":"Nope","namespace":"default","name":"x"}`,
			"no kind \"Nope\"",
		},
		{
			"absent object",
			`{"context":"test","api_group":"apps","api_version":"v1","kind":"Deployment","namespace":"default","name":"absent"}`,
			"no Deployment \"absent\" in namespace default",
		},
		{
			"unknown context",
			`{"context":"nope","api_version":"v1","kind":"Node","name":"node1"}`,
			"unknown context",
		},
		{
			"missing api_version",
			`{"context":"test","kind":"Node","name":"node1"}`,
			"api_version argument is required",
		},
		{
			"missing name",
			`{"context":"test","api_version":"v1","kind":"Node"}`,
			"kind and name arguments are required",
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			res := callTool(t, NewGetArbitraryManifestHandler(c), tc.args)
			if !res.IsError {
				t.Fatalf("expected a tool error, got %+v", res.StructuredContent)
			}
			if !strings.Contains(contentText(t, res), tc.want) {
				t.Errorf("error did not mention %q: %s", tc.want, contentText(t, res))
			}
		})
	}
}
