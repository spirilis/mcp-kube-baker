package tools

import (
	"context"
	"encoding/json"

	"github.com/spirilis/generic-go-mcp/mcp"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	"github.com/spirilis/mcp-kube-baker/internal/kube"
)

type podRow struct {
	Namespace   string `json:"namespace"`
	PodName     string `json:"pod_name"`
	PodIP       string `json:"pod_ip,omitempty"`
	NodeName    string `json:"node_name,omitempty"`
	StatusPhase string `json:"status_phase"`
	// Containers names what kubectl_get_pod_logs can be asked for. Init
	// containers are listed separately because they have logs too and are a
	// common crash-loop culprit, but they are not what a bare log request means.
	Containers     []string `json:"containers"`
	InitContainers []string `json:"init_containers,omitempty"`
}

type podsOutput struct {
	Pods []podRow `json:"pods"`
}

// GetPodsDefinition describes the kubectl_get_pods tool.
func GetPodsDefinition() mcp.Tool {
	return mcp.Tool{
		Name:        "kubectl_get_pods",
		Title:       "List pods",
		Description: "Returns pods on one Kubernetes cluster. Omit namespaces to get every pod in the cluster, or pass one or more namespace names to restrict the listing. Full pod manifests are available as mcp+kubectl://{context}/pod/{namespace}/{name} resources, and each listed container's logs through kubectl_get_pod_logs.",
		InputSchema: json.RawMessage(`{
			"type": "object",
			"properties": {
				"context": {"type": "string", "description": "Kubeconfig context naming the cluster (see kubectl_get_contexts)"},
				"namespaces": {"type": "array", "items": {"type": "string"}, "description": "Optional namespace names; omit for all namespaces"}
			},
			"required": ["context"]
		}`),
		OutputSchema: json.RawMessage(`{
			"type": "object",
			"properties": {
				"pods": {
					"type": "array",
					"items": {
						"type": "object",
						"properties": {
							"namespace": {"type": "string"},
							"pod_name": {"type": "string"},
							"pod_ip": {"type": "string"},
							"node_name": {"type": "string"},
							"status_phase": {"type": "string"},
							"containers": {"type": "array", "items": {"type": "string"}, "description": "Container names, as accepted by kubectl_get_pod_logs"},
							"init_containers": {"type": "array", "items": {"type": "string"}, "description": "Init container names, if any; they have logs too"}
						},
						"required": ["namespace", "pod_name", "status_phase", "containers"]
					}
				}
			},
			"required": ["pods"]
		}`),
		Annotations: readOnly(),
	}
}

// NewGetPodsHandler returns the kubectl_get_pods handler.
func NewGetPodsHandler(c kube.Clients) mcp.ToolFunction {
	return func(ctx context.Context, req *mcp.ToolRequest) (mcp.Result, error) {
		var args struct {
			Context    string   `json:"context"`
			Namespaces []string `json:"namespaces"`
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

		out := podsOutput{Pods: []podRow{}}
		var links []mcp.Content
		for _, ns := range namespacesOrAll(args.Namespaces) {
			list, err := cs.CoreV1().Pods(ns).List(ctx, metav1.ListOptions{})
			if err != nil {
				return mcp.ErrorResultf("failed to list pods in namespace %q on context %q: %v", ns, args.Context, err), nil
			}
			for _, pod := range list.Items {
				out.Pods = append(out.Pods, podRow{
					Namespace:      pod.Namespace,
					PodName:        pod.Name,
					PodIP:          pod.Status.PodIP,
					NodeName:       pod.Spec.NodeName,
					StatusPhase:    string(pod.Status.Phase),
					Containers:     containerNames(pod.Spec.Containers),
					InitContainers: containerNames(pod.Spec.InitContainers),
				})
				links = append(links, mcp.ResourceLinkContent(
					podURI(args.Context, pod.Namespace, pod.Name),
					pod.Name, "Full Pod manifest", "application/json"))
			}
		}
		return jsonResult(out, links)
	}
}

// containerNames extracts just the names — the only thing a caller needs to
// name a container in a kubectl_get_pod_logs call. Always non-nil so the
// containers field marshals as [] rather than null.
func containerNames(containers []corev1.Container) []string {
	names := make([]string, 0, len(containers))
	for _, c := range containers {
		names = append(names, c.Name)
	}
	return names
}
