package tools

import (
	"context"
	"encoding/json"

	"github.com/spirilis/generic-go-mcp/mcp"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	"github.com/spirilis/mcp-kube-baker/internal/kube"
)

type nodeRow struct {
	Name              string            `json:"name"`
	KubernetesVersion string            `json:"kubernetes_version"`
	Labels            map[string]string `json:"labels,omitempty"`
	Allocatable       map[string]string `json:"allocatable,omitempty"`
	Capacity          map[string]string `json:"capacity,omitempty"`
	Addresses         map[string]string `json:"addresses,omitempty"`
}

type nodesOutput struct {
	Nodes []nodeRow `json:"nodes"`
}

// GetNodesDefinition describes the kubectl_get_nodes tool.
func GetNodesDefinition() mcp.Tool {
	return mcp.Tool{
		Name:        "kubectl_get_nodes",
		Title:       "List nodes",
		Description: "Returns the Nodes of one Kubernetes cluster with version, labels, allocatable/capacity resources, and addresses. Full Node manifests are available as mcp+kubectl://{context}/node/{node} resources.",
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
				"nodes": {
					"type": "array",
					"items": {
						"type": "object",
						"properties": {
							"name": {"type": "string"},
							"kubernetes_version": {"type": "string"},
							"labels": {"type": "object", "additionalProperties": {"type": "string"}},
							"allocatable": {"type": "object", "additionalProperties": {"type": "string"}},
							"capacity": {"type": "object", "additionalProperties": {"type": "string"}},
							"addresses": {"type": "object", "additionalProperties": {"type": "string"}}
						},
						"required": ["name", "kubernetes_version"]
					}
				}
			},
			"required": ["nodes"]
		}`),
		Annotations: readOnly(),
	}
}

// NewGetNodesHandler returns the kubectl_get_nodes handler.
func NewGetNodesHandler(c kube.Clients) mcp.ToolFunction {
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

		ctx, cancel := apiCtx(ctx)
		defer cancel()
		list, err := cs.CoreV1().Nodes().List(ctx, metav1.ListOptions{})
		if err != nil {
			return mcp.ErrorResultf("failed to list nodes on context %q: %v", args.Context, err), nil
		}

		out := nodesOutput{Nodes: []nodeRow{}}
		var links []mcp.Content
		for _, node := range list.Items {
			row := nodeRow{
				Name:              node.Name,
				KubernetesVersion: node.Status.NodeInfo.KubeletVersion,
				Labels:            node.Labels,
				Allocatable:       map[string]string{},
				Capacity:          map[string]string{},
				Addresses:         map[string]string{},
			}
			for name, qty := range node.Status.Allocatable {
				row.Allocatable[string(name)] = qty.String()
			}
			for name, qty := range node.Status.Capacity {
				row.Capacity[string(name)] = qty.String()
			}
			for _, addr := range node.Status.Addresses {
				row.Addresses[string(addr.Type)] = addr.Address
			}
			out.Nodes = append(out.Nodes, row)
			links = append(links, mcp.ResourceLinkContent(
				nodeURI(args.Context, node.Name),
				node.Name, "Full Node manifest", "application/json"))
		}
		return jsonResult(out, links)
	}
}
