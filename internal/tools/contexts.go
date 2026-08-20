package tools

import (
	"context"
	"encoding/json"

	"github.com/spirilis/generic-go-mcp/mcp"

	"github.com/spirilis/mcp-kube-baker/internal/kube"
)

type contextsOutput struct {
	Contexts       []string `json:"contexts"`
	CurrentContext string   `json:"current_context,omitempty"`
}

// GetContextsDefinition describes the kubectl_get_contexts tool.
func GetContextsDefinition() mcp.Tool {
	return mcp.Tool{
		Name:        "kubectl_get_contexts",
		Title:       "List kubeconfig contexts",
		Description: "Returns the list of Kubernetes contexts (clusters) this server can reach, and which one is the kubeconfig's current-context. Every other tool takes one of these context names as its context argument.",
		InputSchema: json.RawMessage(`{"type":"object","properties":{}}`),
		OutputSchema: json.RawMessage(`{
			"type": "object",
			"properties": {
				"contexts": {"type": "array", "items": {"type": "string"}},
				"current_context": {"type": "string"}
			},
			"required": ["contexts"]
		}`),
		Annotations: readOnly(),
	}
}

// NewGetContextsHandler returns the kubectl_get_contexts handler.
func NewGetContextsHandler(c kube.Clients) mcp.ToolFunction {
	return func(ctx context.Context, req *mcp.ToolRequest) (mcp.Result, error) {
		names, current := c.Contexts()
		return jsonResult(contextsOutput{Contexts: names, CurrentContext: current}, nil)
	}
}
