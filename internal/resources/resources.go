// Package resources registers mcp-kube-baker's parameterized resources: RFC
// 6570 templates under the mcp+kubectl:// scheme, dispatching resources/read
// of a concrete URI to the matching Kubernetes object's full JSON manifest.
// Requires generic-go-mcp >= v0.7.0 (resource template engine).
package resources

import (
	"context"
	"encoding/json"
	"fmt"
	"sort"
	"strings"

	"github.com/spirilis/generic-go-mcp/mcp"
	k8serrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/kubernetes"

	"github.com/spirilis/mcp-kube-baker/internal/helm"
	"github.com/spirilis/mcp-kube-baker/internal/kube"
)

// Template URI shapes. All variables are single-segment Kubernetes names, so
// plain {var} (which never matches "/") is the right RFC 6570 form throughout.
// The two api-resource shapes live in apiresources.go, next to the reason there
// are two of them.
const (
	PodTemplate     = "mcp+kubectl://{context}/pod/{namespace}/{name}"
	EventTemplate   = "mcp+kubectl://{context}/event/{namespace}/{name}"
	NodeTemplate    = "mcp+kubectl://{context}/node/{node}"
	ServiceTemplate = "mcp+kubectl://{context}/service/{namespace}/{name}"
)

// serviceNameLabel is the well-known label tying an EndpointSlice to its
// Service (discovery.k8s.io/v1).
const serviceNameLabel = "kubernetes.io/service-name"

// RegisterAll registers every resource template and its completion provider
// against the given registry, bound to the given cluster clients.
func RegisterAll(rr *mcp.ResourceRegistry, c kube.Clients) error {
	templates := []struct {
		tmpl mcp.ResourceTemplate
		fn   mcp.ResourceTemplateFunction
	}{
		{
			mcp.ResourceTemplate{
				URITemplate: PodTemplate,
				Name:        "Pod manifest",
				Title:       "Pod manifest",
				Description: "The full JSON manifest of one Pod",
				MimeType:    "application/json",
			},
			readPod(c),
		},
		{
			mcp.ResourceTemplate{
				URITemplate: EventTemplate,
				Name:        "Event manifest",
				Title:       "Event manifest",
				Description: "The full JSON manifest of one Event",
				MimeType:    "application/json",
			},
			readEvent(c),
		},
		{
			mcp.ResourceTemplate{
				URITemplate: NodeTemplate,
				Name:        "Node manifest",
				Title:       "Node manifest",
				Description: "The full JSON manifest of one Node",
				MimeType:    "application/json",
			},
			readNode(c),
		},
		{
			mcp.ResourceTemplate{
				URITemplate: ServiceTemplate,
				Name:        "Service manifest with EndpointSlices",
				Title:       "Service + EndpointSlices",
				Description: "The full JSON manifest of one Service plus all its EndpointSlices, as an items[] array",
				MimeType:    "application/json",
			},
			readService(c),
		},
		{
			mcp.ResourceTemplate{
				URITemplate: HelmValuesTemplate,
				Name:        "Helm release values",
				Title:       "Helm release values",
				Description: "The values one Helm release's current revision was installed with (`helm get values`), alongside its chart's default values",
				MimeType:    "application/json",
			},
			readHelmValues(c),
		},
		// Longest first: the registry's match is first-match-wins, and while
		// these two cannot actually collide (three path segments after
		// "api-resource" versus two), the convention is cheap to keep.
		{
			mcp.ResourceTemplate{
				URITemplate: APIResourceTemplate,
				Name:        "API resource schema",
				Title:       "API resource schema",
				Description: "The OpenAPI V3 schema of one Kind, with every schema it references. Use \"core\" as the apiGroup for core (v1) Kinds. The kind variable also accepts the plural, singular, or short name.",
				MimeType:    "application/json",
			},
			readAPIResource(c),
		},
		{
			mcp.ResourceTemplate{
				URITemplate: CoreAPIResourceTemplate,
				Name:        "Core API resource schema",
				Title:       "Core API resource schema",
				Description: "The OpenAPI V3 schema of one core (v1) Kind, with every schema it references — the group-less spelling of the api-resource template.",
				MimeType:    "application/json",
			},
			readAPIResource(c),
		},
	}

	completer := NewCompleter(c)
	for _, t := range templates {
		if err := rr.RegisterTemplate(t.tmpl, t.fn); err != nil {
			return fmt.Errorf("failed to register template %s: %w", t.tmpl.URITemplate, err)
		}
		if err := rr.SetTemplateCompleter(t.tmpl.URITemplate, completer); err != nil {
			return fmt.Errorf("failed to set completer on %s: %w", t.tmpl.URITemplate, err)
		}
	}
	return nil
}

// clientFor resolves the {context} variable. An unknown context means the URI
// names something outside the keyspace — a not-found, not a server fault.
func clientFor(c kube.Clients, contextName string) (kubernetes.Interface, error) {
	cs, err := c.Client(contextName)
	if err != nil {
		return nil, fmt.Errorf("context %q: %w", contextName, mcp.ErrResourceNotFound)
	}
	return cs, nil
}

// jsonContent marshals a fetched object into the resource content result.
func jsonContent(v interface{}) (mcp.ResourceContentResult, error) {
	b, err := json.MarshalIndent(v, "", "  ")
	if err != nil {
		return mcp.ResourceContentResult{}, fmt.Errorf("failed to marshal manifest: %w", err)
	}
	return mcp.ResourceContentResult{Text: string(b), MimeType: "application/json"}, nil
}

// wrapAPIError translates a Kubernetes API error: NotFound becomes the
// library's ErrResourceNotFound (client sees -32602), anything else stays an
// internal error (-32603).
func wrapAPIError(err error, what string) error {
	if k8serrors.IsNotFound(err) {
		return fmt.Errorf("%s: %w", what, mcp.ErrResourceNotFound)
	}
	return fmt.Errorf("failed to fetch %s: %w", what, err)
}

func readPod(c kube.Clients) mcp.ResourceTemplateFunction {
	return func(ctx context.Context, req *mcp.ResourceReadRequest) (mcp.ResourceContentResult, error) {
		cs, err := clientFor(c, req.Vars["context"])
		if err != nil {
			return mcp.ResourceContentResult{}, err
		}
		ctx, cancel := context.WithTimeout(ctx, kube.APITimeout)
		defer cancel()
		pod, err := cs.CoreV1().Pods(req.Vars["namespace"]).Get(ctx, req.Vars["name"], metav1.GetOptions{})
		if err != nil {
			return mcp.ResourceContentResult{}, wrapAPIError(err, fmt.Sprintf("pod %s/%s", req.Vars["namespace"], req.Vars["name"]))
		}
		pod.TypeMeta = metav1.TypeMeta{Kind: "Pod", APIVersion: "v1"}
		return jsonContent(pod)
	}
}

func readEvent(c kube.Clients) mcp.ResourceTemplateFunction {
	return func(ctx context.Context, req *mcp.ResourceReadRequest) (mcp.ResourceContentResult, error) {
		cs, err := clientFor(c, req.Vars["context"])
		if err != nil {
			return mcp.ResourceContentResult{}, err
		}
		ctx, cancel := context.WithTimeout(ctx, kube.APITimeout)
		defer cancel()
		ev, err := cs.CoreV1().Events(req.Vars["namespace"]).Get(ctx, req.Vars["name"], metav1.GetOptions{})
		if err != nil {
			return mcp.ResourceContentResult{}, wrapAPIError(err, fmt.Sprintf("event %s/%s", req.Vars["namespace"], req.Vars["name"]))
		}
		ev.TypeMeta = metav1.TypeMeta{Kind: "Event", APIVersion: "v1"}
		return jsonContent(ev)
	}
}

func readNode(c kube.Clients) mcp.ResourceTemplateFunction {
	return func(ctx context.Context, req *mcp.ResourceReadRequest) (mcp.ResourceContentResult, error) {
		cs, err := clientFor(c, req.Vars["context"])
		if err != nil {
			return mcp.ResourceContentResult{}, err
		}
		ctx, cancel := context.WithTimeout(ctx, kube.APITimeout)
		defer cancel()
		node, err := cs.CoreV1().Nodes().Get(ctx, req.Vars["node"], metav1.GetOptions{})
		if err != nil {
			return mcp.ResourceContentResult{}, wrapAPIError(err, fmt.Sprintf("node %s", req.Vars["node"]))
		}
		node.TypeMeta = metav1.TypeMeta{Kind: "Node", APIVersion: "v1"}
		return jsonContent(node)
	}
}

func readService(c kube.Clients) mcp.ResourceTemplateFunction {
	return func(ctx context.Context, req *mcp.ResourceReadRequest) (mcp.ResourceContentResult, error) {
		cs, err := clientFor(c, req.Vars["context"])
		if err != nil {
			return mcp.ResourceContentResult{}, err
		}
		ctx, cancel := context.WithTimeout(ctx, kube.APITimeout)
		defer cancel()

		ns, name := req.Vars["namespace"], req.Vars["name"]
		svc, err := cs.CoreV1().Services(ns).Get(ctx, name, metav1.GetOptions{})
		if err != nil {
			return mcp.ResourceContentResult{}, wrapAPIError(err, fmt.Sprintf("service %s/%s", ns, name))
		}
		svc.TypeMeta = metav1.TypeMeta{Kind: "Service", APIVersion: "v1"}

		slices, err := cs.DiscoveryV1().EndpointSlices(ns).List(ctx, metav1.ListOptions{
			LabelSelector: serviceNameLabel + "=" + name,
		})
		if err != nil {
			return mcp.ResourceContentResult{}, fmt.Errorf("failed to list EndpointSlices for service %s/%s: %w", ns, name, err)
		}

		items := []interface{}{svc}
		for i := range slices.Items {
			slice := &slices.Items[i]
			slice.TypeMeta = metav1.TypeMeta{Kind: "EndpointSlice", APIVersion: "discovery.k8s.io/v1"}
			items = append(items, slice)
		}
		return jsonContent(map[string]interface{}{"items": items})
	}
}

// NewCompleter builds the shared completion provider for every template.
// Only variables cheap to enumerate are completed: kubeconfig context names
// (in-memory), namespaces (one bounded API call, only once the client has
// already resolved a context), the api-resource coordinates, which come from
// discovery and are bounded by what the cluster serves, and Helm release names,
// which are one label-selected list per namespace. Object names are an
// unbounded keyspace — those return empty.
func NewCompleter(c kube.Clients) mcp.CompletionProvider {
	return mcp.CompletionFunc(func(ctx context.Context, req *mcp.CompletionRequest) (mcp.CompletionResult, error) {
		switch req.Argument {
		case "context":
			names, _ := c.Contexts()
			return prefixFiltered(names, req.Value), nil
		case "namespace":
			contextName := req.Context["context"]
			if contextName == "" {
				return mcp.CompletionResult{}, nil
			}
			cs, err := c.Client(contextName)
			if err != nil {
				return mcp.CompletionResult{}, nil // unknown context → nothing to offer
			}
			ctx, cancel := context.WithTimeout(ctx, kube.APITimeout)
			defer cancel()
			list, err := cs.CoreV1().Namespaces().List(ctx, metav1.ListOptions{})
			if err != nil {
				return mcp.CompletionResult{}, nil // unreachable cluster → no candidates, not a hard error
			}
			names := make([]string, 0, len(list.Items))
			for _, item := range list.Items {
				names = append(names, item.Name)
			}
			return prefixFiltered(names, req.Value), nil
		case "apiGroup":
			groups, err := serverGroups(c, req.Context["context"])
			if err != nil {
				return mcp.CompletionResult{}, nil
			}
			// The sentinel is offered alongside the real groups: it is the only
			// way to name the core group in the four-variable template.
			names := []string{CoreGroupSentinel}
			for _, g := range groups.Groups {
				if g.Name != "" {
					names = append(names, g.Name)
				}
			}
			return prefixFiltered(names, req.Value), nil
		case "version":
			// On CoreAPIResourceTemplate there is no apiGroup variable at all,
			// so the template itself is what says "core group".
			wanted := req.Context["apiGroup"]
			if req.URITemplate == CoreAPIResourceTemplate {
				wanted = CoreGroupSentinel
			}
			if wanted == "" {
				return mcp.CompletionResult{}, nil
			}
			if wanted == CoreGroupSentinel {
				wanted = ""
			}
			groups, err := serverGroups(c, req.Context["context"])
			if err != nil {
				return mcp.CompletionResult{}, nil
			}
			for _, g := range groups.Groups {
				if g.Name != wanted {
					continue
				}
				names := make([]string, 0, len(g.Versions))
				for _, v := range g.Versions {
					names = append(names, v.Version)
				}
				return prefixFiltered(names, req.Value), nil
			}
			return mcp.CompletionResult{}, nil
		case "kind":
			group := req.Context["apiGroup"]
			if req.URITemplate == CoreAPIResourceTemplate {
				group = CoreGroupSentinel
			}
			version := req.Context["version"]
			if group == "" || version == "" {
				return mcp.CompletionResult{}, nil
			}
			if group == CoreGroupSentinel {
				group = ""
			}
			cs, err := c.Client(req.Context["context"])
			if err != nil {
				return mcp.CompletionResult{}, nil
			}
			list, err := cs.Discovery().ServerResourcesForGroupVersion(kube.GroupVersion(group, version))
			if err != nil {
				return mcp.CompletionResult{}, nil
			}
			var names []string
			for _, res := range list.APIResources {
				if strings.Contains(res.Name, "/") {
					continue // subresources are not addressable Kinds here
				}
				names = append(names, res.Kind)
			}
			return prefixFiltered(names, req.Value), nil
		case "release":
			// Helm releases in one namespace are a bounded, label-selectable
			// set — one list call, the same one `helm list` makes.
			contextName, namespace := req.Context["context"], req.Context["namespace"]
			if contextName == "" || namespace == "" {
				return mcp.CompletionResult{}, nil
			}
			cs, err := c.Client(contextName)
			if err != nil {
				return mcp.CompletionResult{}, nil
			}
			ctx, cancel := context.WithTimeout(ctx, kube.APITimeout)
			defer cancel()
			records, _, err := helm.ListLatest(ctx, cs, namespace)
			if err != nil {
				return mcp.CompletionResult{}, nil
			}
			names := make([]string, 0, len(records))
			for _, rec := range records {
				names = append(names, rec.Release.Name)
			}
			return prefixFiltered(names, req.Value), nil
		default:
			return mcp.CompletionResult{}, nil
		}
	})
}

// serverGroups fetches the API group list for one context, or reports why it
// could not. Every completion caller treats any error as "no candidates".
func serverGroups(c kube.Clients, contextName string) (*metav1.APIGroupList, error) {
	if contextName == "" {
		return nil, fmt.Errorf("no context resolved yet")
	}
	cs, err := c.Client(contextName)
	if err != nil {
		return nil, err
	}
	return cs.Discovery().ServerGroups()
}

// prefixFiltered narrows candidates by the typed prefix and applies the
// protocol's 100-value cap, reporting the true total.
func prefixFiltered(candidates []string, prefix string) mcp.CompletionResult {
	var matches []string
	for _, name := range candidates {
		if strings.HasPrefix(name, prefix) {
			matches = append(matches, name)
		}
	}
	sort.Strings(matches)
	total := len(matches)
	result := mcp.CompletionResult{Values: matches, Total: &total}
	if len(matches) > 100 {
		result.Values = matches[:100]
		result.HasMore = true
	}
	return result
}
