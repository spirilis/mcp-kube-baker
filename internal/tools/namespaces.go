package tools

import (
	"context"
	"encoding/json"

	"github.com/spirilis/generic-go-mcp/mcp"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	"github.com/spirilis/mcp-kube-baker/internal/kube"
)

type namespacesOutput struct {
	Namespaces []string `json:"namespaces"`
}

// GetNamespacesDefinition describes the kubectl_get_namespaces tool.
func GetNamespacesDefinition() mcp.Tool {
	return mcp.Tool{
		Name:        "kubectl_get_namespaces",
		Title:       "List namespaces",
		Description: "Returns the namespaces on one Kubernetes cluster, selected by kubeconfig context name.",
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
				"namespaces": {"type": "array", "items": {"type": "string"}}
			},
			"required": ["namespaces"]
		}`),
		Annotations: readOnly(),
	}
}

// NewGetNamespacesHandler returns the kubectl_get_namespaces handler.
func NewGetNamespacesHandler(c kube.Clients) mcp.ToolFunction {
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
		list, err := cs.CoreV1().Namespaces().List(ctx, metav1.ListOptions{})
		if err != nil {
			return mcp.ErrorResultf("failed to list namespaces on context %q: %v", args.Context, err), nil
		}

		out := namespacesOutput{Namespaces: make([]string, 0, len(list.Items))}
		for _, ns := range list.Items {
			out.Namespaces = append(out.Namespaces, ns.Name)
		}
		return jsonResult(out, nil)
	}
}
