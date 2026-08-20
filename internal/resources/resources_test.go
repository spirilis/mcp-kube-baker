package resources

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"testing"

	"github.com/spirilis/generic-go-mcp/mcp"
	corev1 "k8s.io/api/core/v1"
	discoveryv1 "k8s.io/api/discovery/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/client-go/dynamic"
	dynamicfake "k8s.io/client-go/dynamic/fake"
	"k8s.io/client-go/kubernetes"
	"k8s.io/client-go/kubernetes/fake"
	"k8s.io/client-go/openapi"
	"k8s.io/client-go/openapi/openapitest"

	"github.com/spirilis/mcp-kube-baker/internal/kube"
)

type fakeClients struct {
	contexts []string
	current  string
	sets     map[string]kubernetes.Interface
}

// Dynamic implements kube.Clients. No resource template reaches for the
// dynamic client — the arbitrary-manifest surface is a tool — so this only has
// to satisfy the interface honestly.
func (f *fakeClients) Dynamic(name string) (dynamic.Interface, error) {
	if _, ok := f.sets[name]; !ok {
		return nil, fmt.Errorf("%w: %q", kube.ErrUnknownContext, name)
	}
	return dynamicfake.NewSimpleDynamicClient(runtime.NewScheme()), nil
}

func (f *fakeClients) Contexts() ([]string, string) { return f.contexts, f.current }

func (f *fakeClients) Client(name string) (kubernetes.Interface, error) {
	cs, ok := f.sets[name]
	if !ok {
		return nil, fmt.Errorf("%w: %q", kube.ErrUnknownContext, name)
	}
	return cs, nil
}

// OpenAPI implements kube.Clients. fake.Clientset's discovery panics on
// OpenAPIV3(), which is exactly why this is an interface method: tests serve
// client-go's own embedded api/v1, apps/v1, batch/v1 and discovery.k8s.io/v1
// specs instead.
func (f *fakeClients) OpenAPI(name string) (openapi.Client, error) {
	if _, ok := f.sets[name]; !ok {
		return nil, fmt.Errorf("%w: %q", kube.ErrUnknownContext, name)
	}
	return openapitest.NewEmbeddedFileClient(), nil
}

// discoveryFixture mirrors the group-versions the embedded OpenAPI specs cover,
// so a resolved Kind always has a document behind it.
func discoveryFixture() []*metav1.APIResourceList {
	return []*metav1.APIResourceList{
		{
			GroupVersion: "v1",
			APIResources: []metav1.APIResource{
				{Name: "pods", SingularName: "pod", Kind: "Pod", Namespaced: true, ShortNames: []string{"po"}, Verbs: []string{"get", "list"}},
				{Name: "pods/status", Kind: "Pod", Namespaced: true},
			},
		},
		{
			GroupVersion: "apps/v1",
			APIResources: []metav1.APIResource{
				{Name: "deployments", SingularName: "deployment", Kind: "Deployment", Namespaced: true, ShortNames: []string{"deploy"}, Verbs: []string{"get", "list"}},
				{Name: "statefulsets", SingularName: "statefulset", Kind: "StatefulSet", Namespaced: true, ShortNames: []string{"sts"}, Verbs: []string{"get", "list"}},
			},
		},
	}
}

func newFixture(t *testing.T, objects ...runtime.Object) (*mcp.ResourceRegistry, *fakeClients) {
	t.Helper()
	test := fake.NewClientset(objects...)
	test.Resources = discoveryFixture()
	c := &fakeClients{
		contexts: []string{"prod", "test"},
		current:  "test",
		sets: map[string]kubernetes.Interface{
			"test": test,
			"prod": fake.NewClientset(),
		},
	}
	rr := mcp.NewResourceRegistry()
	if err := RegisterAll(rr, c); err != nil {
		t.Fatalf("RegisterAll: %v", err)
	}
	return rr, c
}

// read dispatches a concrete URI through the registry's template matcher,
// the same path the server's resources/read handler takes.
func read(t *testing.T, rr *mcp.ResourceRegistry, uri string) (mcp.ResourceContentResult, error) {
	t.Helper()
	_, fn, vars, ok := rr.MatchTemplate(uri)
	if !ok {
		t.Fatalf("no template matched %q", uri)
	}
	return fn(context.Background(), &mcp.ResourceReadRequest{URI: uri, Vars: vars})
}

func TestTemplatesRegistered(t *testing.T) {
	rr, _ := newFixture(t)
	want := map[string]bool{
		PodTemplate:             true,
		EventTemplate:           true,
		NodeTemplate:            true,
		ServiceTemplate:         true,
		HelmValuesTemplate:      true,
		APIResourceTemplate:     true,
		CoreAPIResourceTemplate: true,
	}
	got := rr.ListTemplates()
	if len(got) != len(want) {
		t.Fatalf("expected %d templates, got %d", len(want), len(got))
	}
	for _, tmpl := range got {
		if !want[tmpl.URITemplate] {
			t.Errorf("unexpected template registered: %s", tmpl.URITemplate)
		}
		delete(want, tmpl.URITemplate)
	}
	for missing := range want {
		t.Errorf("template not registered: %s", missing)
	}
}

func TestReadPod(t *testing.T) {
	rr, _ := newFixture(t, &corev1.Pod{
		ObjectMeta: metav1.ObjectMeta{Namespace: "default", Name: "mypod"},
		Spec:       corev1.PodSpec{NodeName: "node1"},
	})
	res, err := read(t, rr, "mcp+kubectl://test/pod/default/mypod")
	if err != nil {
		t.Fatalf("read: %v", err)
	}
	if res.MimeType != "application/json" {
		t.Errorf("unexpected mime type %q", res.MimeType)
	}
	var pod corev1.Pod
	if err := json.Unmarshal([]byte(res.Text), &pod); err != nil {
		t.Fatalf("content is not valid Pod JSON: %v", err)
	}
	if pod.Name != "mypod" || pod.Kind != "Pod" || pod.Spec.NodeName != "node1" {
		t.Errorf("unexpected pod content: name=%q kind=%q", pod.Name, pod.Kind)
	}
}

func TestReadPodNotFound(t *testing.T) {
	rr, _ := newFixture(t)
	_, err := read(t, rr, "mcp+kubectl://test/pod/default/absent")
	if !errors.Is(err, mcp.ErrResourceNotFound) {
		t.Fatalf("expected ErrResourceNotFound, got %v", err)
	}
}

func TestReadUnknownContext(t *testing.T) {
	rr, _ := newFixture(t)
	_, err := read(t, rr, "mcp+kubectl://nope/node/node1")
	if !errors.Is(err, mcp.ErrResourceNotFound) {
		t.Fatalf("expected ErrResourceNotFound for unknown context, got %v", err)
	}
}

func TestReadEvent(t *testing.T) {
	rr, _ := newFixture(t, &corev1.Event{
		ObjectMeta: metav1.ObjectMeta{Namespace: "default", Name: "myevent"},
		Reason:     "Started",
	})
	res, err := read(t, rr, "mcp+kubectl://test/event/default/myevent")
	if err != nil {
		t.Fatalf("read: %v", err)
	}
	if !strings.Contains(res.Text, `"Started"`) {
		t.Errorf("event content missing reason: %s", res.Text)
	}
}

func TestReadNode(t *testing.T) {
	rr, _ := newFixture(t, &corev1.Node{ObjectMeta: metav1.ObjectMeta{Name: "node1"}})
	res, err := read(t, rr, "mcp+kubectl://test/node/node1")
	if err != nil {
		t.Fatalf("read: %v", err)
	}
	var node corev1.Node
	if err := json.Unmarshal([]byte(res.Text), &node); err != nil {
		t.Fatalf("content is not valid Node JSON: %v", err)
	}
	if node.Name != "node1" || node.Kind != "Node" {
		t.Errorf("unexpected node content: %q/%q", node.Name, node.Kind)
	}
}

func TestReadServiceWithEndpointSlices(t *testing.T) {
	rr, _ := newFixture(t,
		&corev1.Service{ObjectMeta: metav1.ObjectMeta{Namespace: "default", Name: "web"}},
		&discoveryv1.EndpointSlice{ObjectMeta: metav1.ObjectMeta{
			Namespace: "default", Name: "web-abc",
			Labels: map[string]string{serviceNameLabel: "web"},
		}},
		&discoveryv1.EndpointSlice{ObjectMeta: metav1.ObjectMeta{
			Namespace: "default", Name: "unrelated-xyz",
			Labels: map[string]string{serviceNameLabel: "other"},
		}},
	)
	res, err := read(t, rr, "mcp+kubectl://test/service/default/web")
	if err != nil {
		t.Fatalf("read: %v", err)
	}
	var doc struct {
		Items []json.RawMessage `json:"items"`
	}
	if err := json.Unmarshal([]byte(res.Text), &doc); err != nil {
		t.Fatalf("content is not valid items JSON: %v", err)
	}
	if len(doc.Items) != 2 {
		t.Fatalf("expected service + 1 matching slice = 2 items, got %d", len(doc.Items))
	}
	if !strings.Contains(res.Text, `"web-abc"`) || strings.Contains(res.Text, `"unrelated-xyz"`) {
		t.Errorf("EndpointSlice label filtering wrong: %s", res.Text)
	}
}

func TestCompleteContext(t *testing.T) {
	_, c := newFixture(t)
	completer := NewCompleter(c)

	res, err := completer.Complete(context.Background(), &mcp.CompletionRequest{
		URITemplate: PodTemplate, Argument: "context", Value: "",
	})
	if err != nil {
		t.Fatalf("Complete: %v", err)
	}
	if len(res.Values) != 2 || res.Values[0] != "prod" || res.Values[1] != "test" {
		t.Errorf("unexpected context completions: %v", res.Values)
	}

	res, err = completer.Complete(context.Background(), &mcp.CompletionRequest{
		URITemplate: PodTemplate, Argument: "context", Value: "pr",
	})
	if err != nil {
		t.Fatalf("Complete: %v", err)
	}
	if len(res.Values) != 1 || res.Values[0] != "prod" {
		t.Errorf("prefix filtering failed: %v", res.Values)
	}
}

func TestCompleteNamespaceNeedsContext(t *testing.T) {
	_, c := newFixture(t,
		&corev1.Namespace{ObjectMeta: metav1.ObjectMeta{Name: "default"}},
		&corev1.Namespace{ObjectMeta: metav1.ObjectMeta{Name: "kube-system"}},
	)
	completer := NewCompleter(c)

	// Without a resolved context there is nothing to query.
	res, err := completer.Complete(context.Background(), &mcp.CompletionRequest{
		URITemplate: PodTemplate, Argument: "namespace", Value: "",
	})
	if err != nil {
		t.Fatalf("Complete: %v", err)
	}
	if len(res.Values) != 0 {
		t.Errorf("expected no completions without context, got %v", res.Values)
	}

	// With the context resolved, namespaces come from that cluster.
	res, err = completer.Complete(context.Background(), &mcp.CompletionRequest{
		URITemplate: PodTemplate, Argument: "namespace", Value: "kube",
		Context: map[string]string{"context": "test"},
	})
	if err != nil {
		t.Fatalf("Complete: %v", err)
	}
	if len(res.Values) != 1 || res.Values[0] != "kube-system" {
		t.Errorf("unexpected namespace completions: %v", res.Values)
	}
}

func TestCompleteUnboundedVariableIsEmpty(t *testing.T) {
	_, c := newFixture(t)
	completer := NewCompleter(c)
	res, err := completer.Complete(context.Background(), &mcp.CompletionRequest{
		URITemplate: PodTemplate, Argument: "name", Value: "x",
		Context: map[string]string{"context": "test", "namespace": "default"},
	})
	if err != nil {
		t.Fatalf("Complete: %v", err)
	}
	if len(res.Values) != 0 {
		t.Errorf("expected no completions for pod names, got %v", res.Values)
	}
}

// readAPI is read() plus the JSON decoding every api-resource assertion needs.
func readAPI(t *testing.T, rr *mcp.ResourceRegistry, uri string) (map[string]interface{}, string) {
	t.Helper()
	res, err := read(t, rr, uri)
	if err != nil {
		t.Fatalf("read %s: %v", uri, err)
	}
	var doc map[string]interface{}
	if err := json.Unmarshal([]byte(res.Text), &doc); err != nil {
		t.Fatalf("content is not valid JSON: %v", err)
	}
	return doc, res.Text
}

func schemasOf(t *testing.T, doc map[string]interface{}) map[string]interface{} {
	t.Helper()
	components, ok := doc["components"].(map[string]interface{})
	if !ok {
		t.Fatal("document has no components object")
	}
	schemas, ok := components["schemas"].(map[string]interface{})
	if !ok {
		t.Fatal("document has no components.schemas object")
	}
	return schemas
}

func TestReadAPIResourceSchemaClosure(t *testing.T) {
	rr, _ := newFixture(t)
	doc, _ := readAPI(t, rr, "mcp+kubectl://test/api-resource/apps/v1/Deployment")

	if got := doc["x-kubernetes-root-schema"]; got != "io.k8s.api.apps.v1.Deployment" {
		t.Errorf("unexpected root schema %v", got)
	}
	schemas := schemasOf(t, doc)
	// The seed, plus something only reachable by walking $ref two levels down.
	for _, want := range []string{
		"io.k8s.api.apps.v1.Deployment",
		"io.k8s.api.apps.v1.DeploymentSpec",
		"io.k8s.api.core.v1.PodTemplateSpec",
		"io.k8s.api.core.v1.Container",
	} {
		if _, ok := schemas[want]; !ok {
			t.Errorf("closure is missing %s", want)
		}
	}
	// The closure is a subset, not the whole document: apps/v1 serves Kinds
	// Deployment does not reference.
	if _, ok := schemas["io.k8s.api.apps.v1.DaemonSet"]; ok {
		t.Error("closure leaked an unreferenced Kind (DaemonSet)")
	}

	meta, ok := doc["x-kubernetes-api-resource"].(map[string]interface{})
	if !ok {
		t.Fatal("document has no x-kubernetes-api-resource block")
	}
	if meta["group"] != "apps" || meta["version"] != "v1" || meta["kind"] != "Deployment" ||
		meta["resource"] != "deployments" || meta["namespaced"] != true {
		t.Errorf("unexpected api-resource metadata: %v", meta)
	}
}

func TestReadAPIResourceClosureIsSelfContained(t *testing.T) {
	rr, _ := newFixture(t)
	doc, _ := readAPI(t, rr, "mcp+kubectl://test/api-resource/apps/v1/Deployment")
	schemas := schemasOf(t, doc)

	// Every $ref inside the emitted document must resolve inside it, or the
	// reader is holding a schema it cannot follow.
	for name, schema := range schemas {
		for _, ref := range collectRefs(schema) {
			if _, ok := schemas[ref]; !ok {
				t.Errorf("%s references %s, which is not in the closure", name, ref)
			}
		}
	}
}

func TestReadAPIResourceCoreBothFormsAgree(t *testing.T) {
	rr, _ := newFixture(t)
	sentinel, sentinelText := readAPI(t, rr, "mcp+kubectl://test/api-resource/core/v1/Pod")
	short, shortText := readAPI(t, rr, "mcp+kubectl://test/api-resource/v1/Pod")

	if sentinelText != shortText {
		t.Error("the core sentinel and the short form returned different documents")
	}
	if got := sentinel["x-kubernetes-root-schema"]; got != "io.k8s.api.core.v1.Pod" {
		t.Errorf("unexpected root schema %v", got)
	}
	meta := short["x-kubernetes-api-resource"].(map[string]interface{})
	if meta["group"] != "" {
		t.Errorf("core group should report an empty group name, got %q", meta["group"])
	}
}

func TestReadAPIResourceAlternateSpellings(t *testing.T) {
	rr, _ := newFixture(t)
	_, canonical := readAPI(t, rr, "mcp+kubectl://test/api-resource/apps/v1/Deployment")
	for _, token := range []string{"deployments", "deployment", "deploy", "deployMENT"} {
		_, got := readAPI(t, rr, "mcp+kubectl://test/api-resource/apps/v1/"+token)
		if got != canonical {
			t.Errorf("%q did not resolve to the Deployment schema", token)
		}
	}
}

func TestReadAPIResourceNotFound(t *testing.T) {
	rr, _ := newFixture(t)
	for _, uri := range []string{
		"mcp+kubectl://test/api-resource/apps/v1/Nonexistent",
		"mcp+kubectl://test/api-resource/bogus.io/v1/Thing",
		"mcp+kubectl://test/api-resource/v1/Nonexistent",
		"mcp+kubectl://nope/api-resource/apps/v1/Deployment",
	} {
		if _, err := read(t, rr, uri); !errors.Is(err, mcp.ErrResourceNotFound) {
			t.Errorf("%s: expected ErrResourceNotFound, got %v", uri, err)
		}
	}
}

// The cluster serves apps/v1 StatefulSet, but no OpenAPI document exists for a
// group-version the spec set does not cover. That is still "not here", not a
// server fault — but it must be distinguishable from a resolved Kind.
func TestReadAPIResourceWithoutSchemaDocument(t *testing.T) {
	rr, c := newFixture(t)
	// batch/v1 has an embedded spec but is absent from the discovery fixture,
	// so resolution fails before the document is ever fetched.
	if _, err := read(t, rr, "mcp+kubectl://test/api-resource/batch/v1/Job"); !errors.Is(err, mcp.ErrResourceNotFound) {
		t.Errorf("expected ErrResourceNotFound for an undiscovered group-version, got %v", err)
	}
	if _, err := c.OpenAPI("test"); err != nil {
		t.Fatalf("OpenAPI: %v", err)
	}
}

func TestCompleteAPIResourceVariables(t *testing.T) {
	_, c := newFixture(t)
	completer := NewCompleter(c)
	complete := func(tmpl, arg, value string, vars map[string]string) []string {
		t.Helper()
		res, err := completer.Complete(context.Background(), &mcp.CompletionRequest{
			URITemplate: tmpl, Argument: arg, Value: value, Context: vars,
		})
		if err != nil {
			t.Fatalf("Complete(%s): %v", arg, err)
		}
		return res.Values
	}

	got := complete(APIResourceTemplate, "apiGroup", "", map[string]string{"context": "test"})
	if len(got) != 2 || got[0] != "apps" || got[1] != CoreGroupSentinel {
		t.Errorf("unexpected apiGroup completions: %v", got)
	}

	got = complete(APIResourceTemplate, "version", "", map[string]string{"context": "test", "apiGroup": "apps"})
	if len(got) != 1 || got[0] != "v1" {
		t.Errorf("unexpected apps version completions: %v", got)
	}

	// The sentinel resolves to the core group's versions...
	got = complete(APIResourceTemplate, "version", "", map[string]string{"context": "test", "apiGroup": CoreGroupSentinel})
	if len(got) != 1 || got[0] != "v1" {
		t.Errorf("unexpected core version completions via sentinel: %v", got)
	}
	// ...and so does the short template, which has no apiGroup variable at all.
	got = complete(CoreAPIResourceTemplate, "version", "", map[string]string{"context": "test"})
	if len(got) != 1 || got[0] != "v1" {
		t.Errorf("unexpected core version completions via short template: %v", got)
	}

	got = complete(APIResourceTemplate, "kind", "Dep", map[string]string{"context": "test", "apiGroup": "apps", "version": "v1"})
	if len(got) != 1 || got[0] != "Deployment" {
		t.Errorf("unexpected kind completions: %v", got)
	}

	// Subresources are not Kinds anyone can address.
	got = complete(CoreAPIResourceTemplate, "kind", "", map[string]string{"context": "test", "version": "v1"})
	if len(got) != 1 || got[0] != "Pod" {
		t.Errorf("unexpected core kind completions: %v", got)
	}

	// Nothing to offer until the earlier variables are bound.
	if got = complete(APIResourceTemplate, "version", "", map[string]string{"context": "test"}); len(got) != 0 {
		t.Errorf("expected no version completions without an apiGroup, got %v", got)
	}
	if got = complete(APIResourceTemplate, "kind", "", map[string]string{"context": "test", "apiGroup": "apps"}); len(got) != 0 {
		t.Errorf("expected no kind completions without a version, got %v", got)
	}
}
