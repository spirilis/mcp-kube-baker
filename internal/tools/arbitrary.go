package tools

import (
	"context"
	"encoding/json"
	"errors"
	"strings"

	"github.com/spirilis/generic-go-mcp/mcp"
	k8serrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime/schema"

	"github.com/spirilis/mcp-kube-baker/internal/kube"
)

type arbitraryManifestOutput struct {
	// Resource records what the Kind/group/version arguments actually resolved
	// to, so a model that guessed a short name or a plural can see what it got.
	Resource arbitraryResourceRef   `json:"resource"`
	Manifest map[string]interface{} `json:"manifest"`
}

type arbitraryResourceRef struct {
	APIVersion string `json:"api_version"`
	Group      string `json:"api_group"`
	Version    string `json:"version"`
	Kind       string `json:"kind"`
	Resource   string `json:"resource"`
	Namespaced bool   `json:"namespaced"`
	Namespace  string `json:"namespace,omitempty"`
	Name       string `json:"name"`
}

// GetArbitraryManifestDefinition describes the kubectl_get_arbitrary_manifest tool.
func GetArbitraryManifestDefinition() mcp.Tool {
	return mcp.Tool{
		Name:  "kubectl_get_arbitrary_manifest",
		Title: "Get any resource's manifest",
		Description: "Fetches the full JSON manifest of any single object of any Kind the cluster serves — including CustomResourceDefinitions and the custom resources they define — the way `kubectl get <kind> <name> -o json` does. " +
			"Use kubectl_get_api_resources to discover the api_group, version and kind a cluster serves, and mcp+kubectl://{context}/api-resource/{apiGroup}/{version}/{kind} for that Kind's schema. " +
			"Namespaced Kinds require the namespace argument; cluster-scoped Kinds must omit it. " +
			"The manifest is returned as the API server sent it, minus metadata.managedFields.",
		InputSchema: json.RawMessage(`{
			"type": "object",
			"properties": {
				"context": {"type": "string", "description": "Kubeconfig context naming the cluster (see kubectl_get_contexts)"},
				"api_group": {"type": "string", "description": "API group, e.g. apps or cert-manager.io. Omit it, or pass \"core\", for core (v1) Kinds"},
				"api_version": {"type": "string", "description": "Version within the group, e.g. v1 or v1beta1. A full \"group/version\" is also accepted, in which case api_group may be omitted"},
				"kind": {"type": "string", "description": "Kind to fetch. The plural, singular, or short name is also accepted, e.g. Deployment, deployments, deploy"},
				"name": {"type": "string", "description": "Name of the object"},
				"namespace": {"type": "string", "description": "Namespace of the object; required for namespaced Kinds, must be omitted for cluster-scoped ones"}
			},
			"required": ["context", "api_version", "kind", "name"]
		}`),
		OutputSchema: json.RawMessage(`{
			"type": "object",
			"properties": {
				"resource": {
					"type": "object",
					"properties": {
						"api_version": {"type": "string"},
						"api_group": {"type": "string"},
						"version": {"type": "string"},
						"kind": {"type": "string"},
						"resource": {"type": "string"},
						"namespaced": {"type": "boolean"},
						"namespace": {"type": "string"},
						"name": {"type": "string"}
					},
					"required": ["api_version", "kind", "resource", "namespaced", "name"]
				},
				"manifest": {"type": "object", "description": "The object's full JSON manifest, as the API server returned it"}
			},
			"required": ["resource", "manifest"]
		}`),
		Annotations: readOnly(),
	}
}

// NewGetArbitraryManifestHandler returns the kubectl_get_arbitrary_manifest handler.
func NewGetArbitraryManifestHandler(c kube.Clients) mcp.ToolFunction {
	return func(ctx context.Context, req *mcp.ToolRequest) (mcp.Result, error) {
		var args struct {
			Context    string `json:"context"`
			APIGroup   string `json:"api_group"`
			APIVersion string `json:"api_version"`
			Kind       string `json:"kind"`
			Name       string `json:"name"`
			Namespace  string `json:"namespace"`
		}
		if err := req.BindArguments(&args); err != nil {
			return mcp.ErrorResultf("invalid arguments: %v", err), nil
		}
		if args.Kind == "" || args.Name == "" {
			return mcp.ErrorResultf("the kind and name arguments are required"), nil
		}
		cs, errResult := clientFor(c, args.Context)
		if errResult != nil {
			return errResult, nil
		}

		group, version, errResult := splitGroupVersion(args.APIGroup, args.APIVersion)
		if errResult != nil {
			return errResult, nil
		}

		// Discovery takes no context.Context; client-go bounds it with the rest
		// client's own timeout.
		res, err := kube.ResolveAPIResource(cs.Discovery(), group, version, args.Kind)
		if err != nil {
			switch {
			case errors.Is(err, kube.ErrUnknownGroupVersion):
				return mcp.ErrorResultf("context %q does not serve api group-version %q: use kubectl_get_api_resources to list what it does serve",
					args.Context, kube.GroupVersion(group, version)), nil
			case errors.Is(err, kube.ErrUnknownKind):
				return mcp.ErrorResultf("no kind %q in api group-version %q on context %q: use kubectl_get_api_resources to list the kinds it serves",
					args.Kind, kube.GroupVersion(group, version), args.Context), nil
			default:
				return mcp.ErrorResultf("failed to resolve kind %q on context %q: %v", args.Kind, args.Context, err), nil
			}
		}

		// The scope mismatch is a model-recoverable mistake in both directions,
		// and saying which way it went is what lets the model fix it in one
		// step rather than guessing.
		if res.Namespaced && args.Namespace == "" {
			return mcp.ErrorResultf("%s is namespaced: the namespace argument is required", res.Kind), nil
		}
		if !res.Namespaced && args.Namespace != "" {
			return mcp.ErrorResultf("%s is cluster-scoped: omit the namespace argument", res.Kind), nil
		}

		dc, err := c.Dynamic(args.Context)
		if err != nil {
			return mcp.ErrorResultf("failed to build client for context %q: %v", args.Context, err), nil
		}
		gvr := schema.GroupVersionResource{Group: group, Version: version, Resource: res.Name}

		ctx, cancel := apiCtx(ctx)
		defer cancel()

		// An empty namespace on the dynamic client is the cluster-scoped
		// request, which is exactly what a cluster-scoped Kind has been
		// validated into by this point.
		obj, err := dc.Resource(gvr).Namespace(args.Namespace).Get(ctx, args.Name, metav1.GetOptions{})
		if err != nil {
			if k8serrors.IsNotFound(err) {
				return mcp.ErrorResultf("no %s %q%s on context %q", res.Kind, args.Name, inNamespace(args.Namespace), args.Context), nil
			}
			return mcp.ErrorResultf("failed to get %s %q%s on context %q: %v", res.Kind, args.Name, inNamespace(args.Namespace), args.Context, err), nil
		}

		// Server-side apply bookkeeping is a third of a typical manifest's
		// bytes and tells the reader nothing about the object. kubectl strips
		// it from `get -o json` by default for the same reason.
		unstructured.RemoveNestedField(obj.Object, "metadata", "managedFields")

		out := arbitraryManifestOutput{
			Resource: arbitraryResourceRef{
				APIVersion: kube.GroupVersion(group, version),
				Group:      group,
				Version:    version,
				Kind:       res.Kind,
				Resource:   res.Name,
				Namespaced: res.Namespaced,
				Namespace:  args.Namespace,
				Name:       args.Name,
			},
			Manifest: obj.Object,
		}
		return jsonResult(out, nil)
	}
}

// splitGroupVersion normalizes the group/version pair. api_version is accepted
// either bare ("v1", with the group in api_group) or fully qualified
// ("apps/v1"), because both spellings appear in manifests a model will have
// read, and the qualified one carries the group already.
func splitGroupVersion(apiGroup, apiVersion string) (group, version string, errResult *mcp.ToolCallResult) {
	group = kube.NormalizeGroup(apiGroup)
	version = apiVersion

	if strings.Contains(apiVersion, "/") {
		parts := strings.Split(apiVersion, "/")
		if len(parts) != 2 || parts[0] == "" || parts[1] == "" {
			return "", "", mcp.ErrorResultf("api_version %q is not a valid group/version", apiVersion)
		}
		qualifiedGroup, qualifiedVersion := parts[0], parts[1]
		if group != "" && group != qualifiedGroup {
			return "", "", mcp.ErrorResultf("api_group %q contradicts the group in api_version %q: pass one or the other", apiGroup, apiVersion)
		}
		group, version = qualifiedGroup, qualifiedVersion
	}
	if version == "" {
		return "", "", mcp.ErrorResultf("the api_version argument is required, e.g. \"v1\" or \"apps/v1\"")
	}
	return group, version, nil
}

// inNamespace renders the " in namespace X" clause, or nothing for
// cluster-scoped objects.
func inNamespace(namespace string) string {
	if namespace == "" {
		return ""
	}
	return " in namespace " + namespace
}
