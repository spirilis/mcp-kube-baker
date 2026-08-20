package tools

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"
	"testing"

	"github.com/spirilis/generic-go-mcp/mcp"
	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/resource"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/client-go/discovery"
	"k8s.io/client-go/dynamic"
	dynamicfake "k8s.io/client-go/dynamic/fake"
	"k8s.io/client-go/kubernetes"
	"k8s.io/client-go/kubernetes/fake"
	"k8s.io/client-go/openapi"
	"k8s.io/client-go/openapi/openapitest"
	k8stesting "k8s.io/client-go/testing"

	"github.com/spirilis/mcp-kube-baker/internal/kube"
)

// fakeClients implements kube.Clients over fake clientsets.
type fakeClients struct {
	contexts []string
	current  string
	sets     map[string]kubernetes.Interface
	dyn      map[string]dynamic.Interface
}

func (f *fakeClients) Contexts() ([]string, string) { return f.contexts, f.current }

func (f *fakeClients) Client(name string) (kubernetes.Interface, error) {
	cs, ok := f.sets[name]
	if !ok {
		return nil, fmt.Errorf("%w: %q", kube.ErrUnknownContext, name)
	}
	return cs, nil
}

// Dynamic implements kube.Clients. Contexts with no unstructured fixtures get
// an empty dynamic client, so "not found" is what a Get against them means.
func (f *fakeClients) Dynamic(name string) (dynamic.Interface, error) {
	if _, ok := f.sets[name]; !ok {
		return nil, fmt.Errorf("%w: %q", kube.ErrUnknownContext, name)
	}
	if dc, ok := f.dyn[name]; ok {
		return dc, nil
	}
	dc := dynamicfake.NewSimpleDynamicClient(runtime.NewScheme())
	f.dyn[name] = dc
	return dc, nil
}

// withDynamic installs unstructured fixtures on the "test" context's dynamic
// client.
func (f *fakeClients) withDynamic(objects ...runtime.Object) *fakeClients {
	f.dyn["test"] = dynamicfake.NewSimpleDynamicClient(runtime.NewScheme(), objects...)
	return f
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

func newFakeClients(objects ...runtime.Object) *fakeClients {
	test := fake.NewClientset(objects...)
	test.Resources = discoveryFixture()
	return &fakeClients{
		contexts: []string{"other", "test"},
		current:  "test",
		sets: map[string]kubernetes.Interface{
			"test":  test,
			"other": fake.NewClientset(),
		},
		dyn: map[string]dynamic.Interface{},
	}
}

// discoveryFixture is what the "test" cluster claims to serve. It deliberately
// includes a subresource, a short name, and a Kind served under two versions of
// one group, since those are the three things aggregate() has to get right.
func discoveryFixture() []*metav1.APIResourceList {
	return []*metav1.APIResourceList{
		{
			GroupVersion: "v1",
			APIResources: []metav1.APIResource{
				{Name: "pods", SingularName: "pod", Kind: "Pod", Namespaced: true, ShortNames: []string{"po"}, Verbs: []string{"get", "list"}},
				{Name: "pods/status", Kind: "Pod", Namespaced: true},
				{Name: "nodes", SingularName: "node", Kind: "Node", Namespaced: false, ShortNames: []string{"no"}, Verbs: []string{"get", "list"}},
			},
		},
		{
			GroupVersion: "apps/v1",
			APIResources: []metav1.APIResource{
				{Name: "deployments", SingularName: "deployment", Kind: "Deployment", Namespaced: true, ShortNames: []string{"deploy"}, Verbs: []string{"get", "list"}},
				{Name: "deployments/scale", Kind: "Scale", Namespaced: true},
			},
		},
		{
			GroupVersion: "apps/v1beta1",
			APIResources: []metav1.APIResource{
				{Name: "deployments", SingularName: "deployment", Kind: "Deployment", Namespaced: true, Verbs: []string{"get", "list"}},
			},
		},
	}
}

func callTool(t *testing.T, fn mcp.ToolFunction, args string) *mcp.ToolCallResult {
	t.Helper()
	res, err := fn(context.Background(), &mcp.ToolRequest{Arguments: json.RawMessage(args)})
	if err != nil {
		t.Fatalf("tool returned protocol error: %v", err)
	}
	tcr, ok := res.(*mcp.ToolCallResult)
	if !ok {
		t.Fatalf("expected *mcp.ToolCallResult, got %T", res)
	}
	return tcr
}

// contentText joins the text blocks of a tool result — what the model actually
// reads, and the only place a tool error's explanation lives.
func contentText(t *testing.T, res *mcp.ToolCallResult) string {
	t.Helper()
	var parts []string
	for _, c := range res.Content {
		if c.Type == "text" {
			parts = append(parts, c.Text)
		}
	}
	return strings.Join(parts, "\n")
}

func pod(ns, name, ip, node string, phase corev1.PodPhase) *corev1.Pod {
	p := podWithContainers(ns, name, []string{"app"}, nil)
	p.Spec.NodeName = node
	p.Status = corev1.PodStatus{PodIP: ip, Phase: phase}
	return p
}

// podWithContainers builds a pod whose container names are the point — the
// shape kubectl_get_pods reports and kubectl_get_pod_logs resolves against.
func podWithContainers(ns, name string, containers, initContainers []string) *corev1.Pod {
	p := &corev1.Pod{ObjectMeta: metav1.ObjectMeta{Namespace: ns, Name: name}}
	for _, c := range containers {
		p.Spec.Containers = append(p.Spec.Containers, corev1.Container{Name: c})
	}
	for _, c := range initContainers {
		p.Spec.InitContainers = append(p.Spec.InitContainers, corev1.Container{Name: c})
	}
	return p
}

func TestGetContexts(t *testing.T) {
	c := newFakeClients()
	res := callTool(t, NewGetContextsHandler(c), `{}`)
	if res.IsError {
		t.Fatalf("unexpected tool error: %+v", res.Content)
	}
	out, ok := res.StructuredContent.(contextsOutput)
	if !ok {
		t.Fatalf("unexpected StructuredContent type %T", res.StructuredContent)
	}
	if len(out.Contexts) != 2 || out.Contexts[0] != "other" || out.Contexts[1] != "test" {
		t.Errorf("unexpected contexts: %v", out.Contexts)
	}
	if out.CurrentContext != "test" {
		t.Errorf("unexpected current context: %q", out.CurrentContext)
	}
}

func TestUnknownContextIsToolError(t *testing.T) {
	c := newFakeClients()
	res := callTool(t, NewGetPodsHandler(c), `{"context":"nope"}`)
	if !res.IsError {
		t.Fatal("expected IsError for unknown context")
	}
}

func TestMissingContextIsToolError(t *testing.T) {
	c := newFakeClients()
	res := callTool(t, NewGetNodesHandler(c), `{}`)
	if !res.IsError {
		t.Fatal("expected IsError for missing context argument")
	}
}

func TestMalformedArgumentsIsToolError(t *testing.T) {
	c := newFakeClients()
	res := callTool(t, NewGetPodsHandler(c), `{"context":123}`)
	if !res.IsError {
		t.Fatal("expected IsError for malformed arguments")
	}
}

func TestGetNamespaces(t *testing.T) {
	c := newFakeClients(
		&corev1.Namespace{ObjectMeta: metav1.ObjectMeta{Name: "default"}},
		&corev1.Namespace{ObjectMeta: metav1.ObjectMeta{Name: "kube-system"}},
	)
	res := callTool(t, NewGetNamespacesHandler(c), `{"context":"test"}`)
	if res.IsError {
		t.Fatalf("unexpected tool error: %+v", res.Content)
	}
	out := res.StructuredContent.(namespacesOutput)
	if len(out.Namespaces) != 2 {
		t.Errorf("expected 2 namespaces, got %v", out.Namespaces)
	}
}

func TestGetPodsAllNamespaces(t *testing.T) {
	c := newFakeClients(
		pod("a", "pod-a", "10.0.0.1", "node1", corev1.PodRunning),
		pod("b", "pod-b", "10.0.0.2", "node2", corev1.PodPending),
		pod("c", "pod-c", "10.0.0.3", "node1", corev1.PodRunning),
	)
	res := callTool(t, NewGetPodsHandler(c), `{"context":"test"}`)
	if res.IsError {
		t.Fatalf("unexpected tool error: %+v", res.Content)
	}
	out := res.StructuredContent.(podsOutput)
	if len(out.Pods) != 3 {
		t.Fatalf("expected 3 pods, got %d", len(out.Pods))
	}
	// One ResourceLink per pod, after the JSON text block.
	if len(res.Content) != 1+3 {
		t.Errorf("expected 4 content blocks (1 text + 3 links), got %d", len(res.Content))
	}
}

func TestGetPodsMultiNamespaceMerge(t *testing.T) {
	c := newFakeClients(
		pod("a", "pod-a", "10.0.0.1", "node1", corev1.PodRunning),
		pod("b", "pod-b", "10.0.0.2", "node2", corev1.PodPending),
		pod("c", "pod-c", "10.0.0.3", "node1", corev1.PodRunning),
	)
	res := callTool(t, NewGetPodsHandler(c), `{"context":"test","namespaces":["a","b"]}`)
	if res.IsError {
		t.Fatalf("unexpected tool error: %+v", res.Content)
	}
	out := res.StructuredContent.(podsOutput)
	if len(out.Pods) != 2 {
		t.Fatalf("expected 2 pods from namespaces a+b, got %d", len(out.Pods))
	}
	row := out.Pods[0]
	if row.Namespace != "a" || row.PodName != "pod-a" || row.PodIP != "10.0.0.1" ||
		row.NodeName != "node1" || row.StatusPhase != "Running" {
		t.Errorf("unexpected pod row: %+v", row)
	}
}

func TestGetPodsReportsContainerNames(t *testing.T) {
	c := newFakeClients(
		podWithContainers("a", "sidecar-pod", []string{"app", "exporter"}, []string{"wait-for-db"}),
		podWithContainers("a", "plain-pod", []string{"app"}, nil),
	)
	res := callTool(t, NewGetPodsHandler(c), `{"context":"test","namespaces":["a"]}`)
	if res.IsError {
		t.Fatalf("unexpected tool error: %+v", res.Content)
	}
	out := res.StructuredContent.(podsOutput)
	rows := map[string]podRow{}
	for _, row := range out.Pods {
		rows[row.PodName] = row
	}

	sidecar := rows["sidecar-pod"]
	if got := strings.Join(sidecar.Containers, ","); got != "app,exporter" {
		t.Errorf("unexpected containers: %q", got)
	}
	if got := strings.Join(sidecar.InitContainers, ","); got != "wait-for-db" {
		t.Errorf("unexpected init_containers: %q", got)
	}

	plain := rows["plain-pod"]
	if got := strings.Join(plain.Containers, ","); got != "app" {
		t.Errorf("unexpected containers: %q", got)
	}
	// A pod with no init containers must omit the field rather than emit null.
	if len(plain.InitContainers) != 0 {
		t.Errorf("expected no init_containers, got %v", plain.InitContainers)
	}
	b, err := json.Marshal(plain)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(b), "init_containers") {
		t.Errorf("init_containers should be omitted when empty: %s", b)
	}
}

func TestGetEventsNamespaceFilter(t *testing.T) {
	ts := metav1.Now()
	c := newFakeClients(
		&corev1.Event{
			ObjectMeta:     metav1.ObjectMeta{Namespace: "a", Name: "ev-a"},
			Count:          3,
			FirstTimestamp: ts,
			Reason:         "Started",
			Message:        "Started container",
			Type:           "Normal",
		},
		&corev1.Event{
			ObjectMeta: metav1.ObjectMeta{Namespace: "b", Name: "ev-b"},
			Reason:     "Failed",
			Type:       "Warning",
		},
	)

	res := callTool(t, NewGetEventsHandler(c), `{"context":"test","namespace":"a"}`)
	out := res.StructuredContent.(eventsOutput)
	if len(out.Events) != 1 || out.Events[0].Name != "ev-a" {
		t.Fatalf("expected only ev-a, got %+v", out.Events)
	}
	if out.Events[0].Count != 3 || out.Events[0].FirstTimestamp == "" {
		t.Errorf("unexpected event row: %+v", out.Events[0])
	}

	res = callTool(t, NewGetEventsHandler(c), `{"context":"test"}`)
	out = res.StructuredContent.(eventsOutput)
	if len(out.Events) != 2 {
		t.Errorf("expected 2 events across all namespaces, got %d", len(out.Events))
	}
}

func TestGetNodes(t *testing.T) {
	c := newFakeClients(&corev1.Node{
		ObjectMeta: metav1.ObjectMeta{
			Name:   "node1",
			Labels: map[string]string{"kubernetes.io/arch": "amd64"},
		},
		Status: corev1.NodeStatus{
			NodeInfo: corev1.NodeSystemInfo{KubeletVersion: "v1.36.3"},
			Allocatable: corev1.ResourceList{
				corev1.ResourceCPU: resource.MustParse("4"),
			},
			Capacity: corev1.ResourceList{
				corev1.ResourceCPU: resource.MustParse("4"),
			},
			Addresses: []corev1.NodeAddress{
				{Type: corev1.NodeInternalIP, Address: "192.168.1.10"},
			},
		},
	})
	res := callTool(t, NewGetNodesHandler(c), `{"context":"test"}`)
	out := res.StructuredContent.(nodesOutput)
	if len(out.Nodes) != 1 {
		t.Fatalf("expected 1 node, got %d", len(out.Nodes))
	}
	n := out.Nodes[0]
	if n.Name != "node1" || n.KubernetesVersion != "v1.36.3" {
		t.Errorf("unexpected node identity: %+v", n)
	}
	if n.Allocatable["cpu"] != "4" || n.Capacity["cpu"] != "4" {
		t.Errorf("unexpected resources: alloc=%v cap=%v", n.Allocatable, n.Capacity)
	}
	if n.Addresses["InternalIP"] != "192.168.1.10" {
		t.Errorf("unexpected addresses: %v", n.Addresses)
	}
	if n.Labels["kubernetes.io/arch"] != "amd64" {
		t.Errorf("unexpected labels: %v", n.Labels)
	}
}

func TestGetServices(t *testing.T) {
	policy := corev1.IPFamilyPolicySingleStack
	itp := corev1.ServiceInternalTrafficPolicyCluster
	c := newFakeClients(&corev1.Service{
		ObjectMeta: metav1.ObjectMeta{Namespace: "default", Name: "web"},
		Spec: corev1.ServiceSpec{
			Selector:              map[string]string{"app": "web"},
			Type:                  corev1.ServiceTypeClusterIP,
			IPFamilies:            []corev1.IPFamily{corev1.IPv4Protocol},
			IPFamilyPolicy:        &policy,
			InternalTrafficPolicy: &itp,
			Ports: []corev1.ServicePort{
				{Name: "http", Protocol: corev1.ProtocolTCP, Port: 80},
			},
		},
	})
	res := callTool(t, NewGetServicesHandler(c), `{"context":"test","namespaces":["default"]}`)
	out := res.StructuredContent.(servicesOutput)
	if len(out.Services) != 1 {
		t.Fatalf("expected 1 service, got %d", len(out.Services))
	}
	svc := out.Services[0]
	if svc.Type != "ClusterIP" || svc.Selector["app"] != "web" ||
		svc.IPFamilyPolicy != "SingleStack" || svc.InternalTrafficPolicy != "Cluster" {
		t.Errorf("unexpected service row: %+v", svc)
	}
	if len(svc.Ports) != 1 || svc.Ports[0].Port != 80 || svc.Ports[0].Protocol != "TCP" {
		t.Errorf("unexpected ports: %+v", svc.Ports)
	}
}

func TestResourceLinkURIEscaping(t *testing.T) {
	uri := podURI("ctx with space", "default", "my-pod")
	want := "mcp+kubectl://ctx%20with%20space/pod/default/my-pod"
	if uri != want {
		t.Errorf("podURI = %q, want %q", uri, want)
	}
}

func TestGetAPIResources(t *testing.T) {
	c := newFakeClients()
	res := callTool(t, NewGetAPIResourcesHandler(c), `{"context":"test"}`)
	if res.IsError {
		t.Fatalf("unexpected tool error: %+v", res.Content)
	}
	out, ok := res.StructuredContent.(apiResourcesOutput)
	if !ok {
		t.Fatalf("unexpected StructuredContent type %T", res.StructuredContent)
	}

	byKind := map[string]apiResourceRow{}
	for _, row := range out.APIResources {
		byKind[row.APIGroup+"/"+row.Kind] = row
	}

	// A Kind served under two versions of one group collapses to one row.
	deploy, ok := byKind["apps/Deployment"]
	if !ok {
		t.Fatal("apps/Deployment missing from the catalog")
	}
	if len(deploy.Versions) != 2 || deploy.Versions[0] != "v1" || deploy.Versions[1] != "v1beta1" {
		t.Errorf("unexpected Deployment versions: %v", deploy.Versions)
	}
	if deploy.Name != "deployments" || !deploy.Namespaced || len(deploy.ShortNames) != 1 || deploy.ShortNames[0] != "deploy" {
		t.Errorf("unexpected Deployment row: %+v", deploy)
	}

	pod, ok := byKind["/Pod"]
	if !ok {
		t.Fatal("core Pod missing from the catalog")
	}
	if pod.APIGroup != "" {
		t.Errorf("core group should report an empty api_group, got %q", pod.APIGroup)
	}
	if node := byKind["/Node"]; node.Namespaced {
		t.Error("Node should not be namespaced")
	}

	// pods/status and deployments/scale are operations, not Kinds.
	for _, row := range out.APIResources {
		if strings.Contains(row.Name, "/") {
			t.Errorf("subresource leaked into the catalog: %s", row.Name)
		}
	}
	if _, ok := byKind["apps/Scale"]; ok {
		t.Error("deployments/scale leaked in as a Scale Kind")
	}
	if len(out.APIResources) != 3 {
		t.Errorf("expected Pod, Node and Deployment, got %d rows", len(out.APIResources))
	}
}

func TestGetAPIResourcesResourceLinks(t *testing.T) {
	c := newFakeClients()
	res := callTool(t, NewGetAPIResourcesHandler(c), `{"context":"test"}`)

	links := map[string]bool{}
	for _, content := range res.Content {
		if content.Type == "resource_link" {
			links[content.URI] = true
		}
	}
	// Core Kinds are linked through the sentinel, since the empty group name is
	// not something a URI path segment can hold.
	for _, want := range []string{
		"mcp+kubectl://test/api-resource/core/v1/Pod",
		"mcp+kubectl://test/api-resource/core/v1/Node",
		"mcp+kubectl://test/api-resource/apps/v1/Deployment",
	} {
		if !links[want] {
			t.Errorf("missing resource link %s (got %v)", want, links)
		}
	}
}

func TestAPIResourceURIEscaping(t *testing.T) {
	if got := apiResourceURI("test", "", "v1", "Pod"); got != "mcp+kubectl://test/api-resource/core/v1/Pod" {
		t.Errorf("empty group should use the sentinel, got %q", got)
	}
	if got := apiResourceURI("my ctx", "discovery.k8s.io", "v1", "EndpointSlice"); got != "mcp+kubectl://my%20ctx/api-resource/discovery.k8s.io/v1/EndpointSlice" {
		t.Errorf("unexpected escaping: %q", got)
	}
}

// A cluster with a broken aggregated APIService fails discovery for that group
// alone. Blanking the whole catalog over it would be the wrong trade — kubectl
// api-resources reports what it can and names what it could not.
func TestGetAPIResourcesPartialDiscoveryFailure(t *testing.T) {
	c := newFakeClients()
	test := c.sets["test"].(*fake.Clientset)
	test.PrependReactor("get", "group", func(k8stesting.Action) (bool, runtime.Object, error) {
		return true, nil, &discovery.ErrGroupDiscoveryFailed{
			Groups: map[schema.GroupVersion]error{
				{Group: "metrics.k8s.io", Version: "v1beta1"}: fmt.Errorf("the server is currently unable to handle the request"),
			},
		}
	})

	res := callTool(t, NewGetAPIResourcesHandler(c), `{"context":"test"}`)
	if res.IsError {
		t.Fatalf("partial discovery failure should not fail the tool: %+v", res.Content)
	}
	out := res.StructuredContent.(apiResourcesOutput)
	if len(out.Warnings) != 1 || !strings.Contains(out.Warnings[0], "metrics.k8s.io/v1beta1") {
		t.Errorf("unexpected warnings: %v", out.Warnings)
	}
}

// Anything that is not a per-group failure is a real fault and should surface
// as a tool error the model can act on.
func TestGetAPIResourcesHardDiscoveryFailure(t *testing.T) {
	c := newFakeClients()
	test := c.sets["test"].(*fake.Clientset)
	test.PrependReactor("get", "group", func(k8stesting.Action) (bool, runtime.Object, error) {
		return true, nil, fmt.Errorf("connection refused")
	})
	if res := callTool(t, NewGetAPIResourcesHandler(c), `{"context":"test"}`); !res.IsError {
		t.Fatal("expected IsError when discovery fails outright")
	}
}
