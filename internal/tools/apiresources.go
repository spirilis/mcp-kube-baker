package tools

import (
	"context"
	"encoding/json"
	"sort"
	"strings"

	"github.com/spirilis/generic-go-mcp/mcp"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/client-go/discovery"

	"github.com/spirilis/mcp-kube-baker/internal/kube"
)

type apiResourceRow struct {
	Name             string   `json:"name"`
	Kind             string   `json:"kind"`
	APIGroup         string   `json:"api_group"`
	ShortNames       []string `json:"short_names,omitempty"`
	Versions         []string `json:"versions"`
	PreferredVersion string   `json:"preferred_version,omitempty"`
	Namespaced       bool     `json:"namespaced"`
	Verbs            []string `json:"verbs,omitempty"`
}

type apiResourcesOutput struct {
	APIResources []apiResourceRow `json:"api_resources"`
	Warnings     []string         `json:"warnings,omitempty"`
}

// GetAPIResourcesDefinition describes the kubectl_get_api_resources tool.
func GetAPIResourcesDefinition() mcp.Tool {
	return mcp.Tool{
		Name:        "kubectl_get_api_resources",
		Title:       "List API resources",
		Description: "Returns every API resource one Kubernetes cluster serves — Kind, plural name, API group, short names, and the versions available — including CustomResourceDefinitions, which differ per cluster. The OpenAPI schema of any one of them is available as an mcp+kubectl://{context}/api-resource/{apiGroup}/{version}/{kind} resource, with \"core\" as the apiGroup for core (v1) Kinds.",
		InputSchema: json.RawMessage(`{
			"type": "object",
			"properties": {
				"context": {"type": "string", "description": "Kubeconfig context naming the cluster (see kubectl_get_contexts)"}
			},
			"required": ["context"]
		}`),
		OutputSchema: json.RawMessage(`{
			"type": "object",
			"properties": {
				"api_resources": {
					"type": "array",
					"items": {
						"type": "object",
						"properties": {
							"name": {"type": "string"},
							"kind": {"type": "string"},
							"api_group": {"type": "string"},
							"short_names": {"type": "array", "items": {"type": "string"}},
							"versions": {"type": "array", "items": {"type": "string"}},
							"preferred_version": {"type": "string"},
							"namespaced": {"type": "boolean"},
							"verbs": {"type": "array", "items": {"type": "string"}}
						},
						"required": ["name", "kind", "api_group", "versions", "namespaced"]
					}
				},
				"warnings": {"type": "array", "items": {"type": "string"}}
			},
			"required": ["api_resources"]
		}`),
		Annotations: readOnly(),
	}
}

// NewGetAPIResourcesHandler returns the kubectl_get_api_resources handler.
func NewGetAPIResourcesHandler(c kube.Clients) mcp.ToolFunction {
	return func(ctx context.Context, req *mcp.ToolRequest) (mcp.Result, error) {
		var args struct {
			Context string `json:"context"`
		}
		if err := req.BindArguments(&args); err != nil {
			return mcp.ErrorResultf("invalid arguments: %v", err), nil
		}
		cs, errResult := clientFor(c, args.Context)
		if errResult != nil {
			return errResult, nil
		}

		// Discovery takes no context.Context; client-go bounds it with the rest
		// client's own timeout.
		groups, lists, err := cs.Discovery().ServerGroupsAndResources()
		var warnings []string
		if err != nil {
			// One unreachable aggregated APIService should not blank out the
			// whole catalog — report what the cluster did answer, and say which
			// groups it did not. This is what kubectl api-resources does.
			failed, ok := discovery.GroupDiscoveryFailedErrorGroups(err)
			if !ok {
				return mcp.ErrorResultf("failed to discover api-resources on context %q: %v", args.Context, err), nil
			}
			for gv, gvErr := range failed {
				warnings = append(warnings, "api group-version "+gv.String()+" is unavailable: "+gvErr.Error())
			}
			sort.Strings(warnings)
		}

		out := apiResourcesOutput{APIResources: aggregate(groups, lists), Warnings: warnings}
		var links []mcp.Content
		for _, row := range out.APIResources {
			version := row.PreferredVersion
			if version == "" {
				version = row.Versions[0]
			}
			links = append(links, mcp.ResourceLinkContent(
				apiResourceURI(args.Context, row.APIGroup, version, row.Kind),
				row.Kind, "OpenAPI schema for "+row.Kind, "application/json"))
		}
		return jsonResult(out, links)
	}
}

// aggregate folds the per-group-version resource lists into one row per
// (group, Kind), collecting the versions each Kind appears under. Subresources
// ("pods/status", "deployments/scale") are dropped: they are operations on a
// Kind, not Kinds of their own, and none of them has a schema of its own to
// link to.
func aggregate(groups []*metav1.APIGroup, lists []*metav1.APIResourceList) []apiResourceRow {
	preferred := make(map[string]string, len(groups))
	for _, g := range groups {
		preferred[g.Name] = g.PreferredVersion.Version
	}

	type key struct{ group, kind string }
	rows := map[key]*apiResourceRow{}
	var order []key

	for _, list := range lists {
		gv, err := schema.ParseGroupVersion(list.GroupVersion)
		if err != nil {
			continue
		}
		for _, res := range list.APIResources {
			if strings.Contains(res.Name, "/") {
				continue
			}
			k := key{gv.Group, res.Kind}
			row, ok := rows[k]
			if !ok {
				row = &apiResourceRow{
					Name:             res.Name,
					Kind:             res.Kind,
					APIGroup:         gv.Group,
					ShortNames:       res.ShortNames,
					PreferredVersion: preferred[gv.Group],
					Namespaced:       res.Namespaced,
					Verbs:            res.Verbs,
				}
				rows[k] = row
				order = append(order, k)
			}
			row.Versions = append(row.Versions, gv.Version)
		}
	}

	sort.Slice(order, func(i, j int) bool {
		if order[i].group != order[j].group {
			return order[i].group < order[j].group
		}
		return order[i].kind < order[j].kind
	})
	out := make([]apiResourceRow, 0, len(order))
	for _, k := range order {
		row := rows[k]
		sort.Strings(row.Versions)
		out = append(out, *row)
	}
	return out
}
