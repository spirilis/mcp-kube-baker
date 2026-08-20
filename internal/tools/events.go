package tools

import (
	"context"
	"encoding/json"
	"time"

	"github.com/spirilis/generic-go-mcp/mcp"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	"github.com/spirilis/mcp-kube-baker/internal/kube"
)

type eventRow struct {
	Count          int32  `json:"count,omitempty"`
	FirstTimestamp string `json:"firstTimestamp,omitempty"`
	Namespace      string `json:"namespace"`
	Name           string `json:"name"`
	Reason         string `json:"reason,omitempty"`
	Message        string `json:"message,omitempty"`
	Type           string `json:"type,omitempty"`
}

type eventsOutput struct {
	Events []eventRow `json:"events"`
}

// GetEventsDefinition describes the kubectl_get_events tool.
func GetEventsDefinition() mcp.Tool {
	return mcp.Tool{
		Name:        "kubectl_get_events",
		Title:       "List events",
		Description: "Returns Events on one Kubernetes cluster, optionally restricted to a single namespace (omit namespace for all namespaces). Full Event manifests are available as mcp+kubectl://{context}/event/{namespace}/{name} resources.",
		InputSchema: json.RawMessage(`{
			"type": "object",
			"properties": {
				"context": {"type": "string", "description": "Kubeconfig context naming the cluster (see kubectl_get_contexts)"},
				"namespace": {"type": "string", "description": "Optional namespace; omit for all namespaces"}
			},
			"required": ["context"]
		}`),
		OutputSchema: json.RawMessage(`{
			"type": "object",
			"properties": {
				"events": {
					"type": "array",
					"items": {
						"type": "object",
						"properties": {
							"count": {"type": "integer"},
							"firstTimestamp": {"type": "string"},
							"namespace": {"type": "string"},
							"name": {"type": "string"},
							"reason": {"type": "string"},
							"message": {"type": "string"},
							"type": {"type": "string"}
						},
						"required": ["namespace", "name"]
					}
				}
			},
			"required": ["events"]
		}`),
		Annotations: readOnly(),
	}
}

// NewGetEventsHandler returns the kubectl_get_events handler.
func NewGetEventsHandler(c kube.Clients) mcp.ToolFunction {
	return func(ctx context.Context, req *mcp.ToolRequest) (mcp.Result, error) {
		var args struct {
			Context   string `json:"context"`
			Namespace string `json:"namespace"`
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
		list, err := cs.CoreV1().Events(args.Namespace).List(ctx, metav1.ListOptions{})
		if err != nil {
			return mcp.ErrorResultf("failed to list events on context %q: %v", args.Context, err), nil
		}

		out := eventsOutput{Events: []eventRow{}}
		var links []mcp.Content
		for _, ev := range list.Items {
			row := eventRow{
				Count:     ev.Count,
				Namespace: ev.Namespace,
				Name:      ev.Name,
				Reason:    ev.Reason,
				Message:   ev.Message,
				Type:      ev.Type,
			}
			if !ev.FirstTimestamp.IsZero() {
				row.FirstTimestamp = ev.FirstTimestamp.Format(time.RFC3339)
			}
			out.Events = append(out.Events, row)
			links = append(links, mcp.ResourceLinkContent(
				eventURI(args.Context, ev.Namespace, ev.Name),
				ev.Name, "Full Event manifest", "application/json"))
		}
		return jsonResult(out, links)
	}
}
