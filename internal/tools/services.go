package tools

import (
	"context"
	"encoding/json"

	"github.com/spirilis/generic-go-mcp/mcp"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	"github.com/spirilis/mcp-kube-baker/internal/kube"
)

type servicePort struct {
	Name       string `json:"name,omitempty"`
	Protocol   string `json:"protocol,omitempty"`
	Port       int32  `json:"port"`
	TargetPort string `json:"target_port,omitempty"`
	NodePort   int32  `json:"node_port,omitempty"`
}

type serviceRow struct {
	Namespace             string            `json:"namespace"`
	Name                  string            `json:"name"`
	Selector              map[string]string `json:"selector,omitempty"`
	Type                  string            `json:"type"`
	IPFamilies            []string          `json:"ipFamilies,omitempty"`
	IPFamilyPolicy        string            `json:"ipFamilyPolicy,omitempty"`
	InternalTrafficPolicy string            `json:"internalTrafficPolicy,omitempty"`
	ExternalTrafficPolicy string            `json:"externalTrafficPolicy,omitempty"`
	Ports                 []servicePort     `json:"ports,omitempty"`
}

type servicesOutput struct {
	Services []serviceRow `json:"services"`
}

// GetServicesDefinition describes the kubectl_get_services tool.
func GetServicesDefinition() mcp.Tool {
	return mcp.Tool{
		Name:        "kubectl_get_services",
		Title:       "List services",
		Description: "Returns Service objects on one Kubernetes cluster. Omit namespaces for all namespaces, or pass one or more namespace names. Full Service manifests (with their EndpointSlices) are available as mcp+kubectl://{context}/service/{namespace}/{name} resources.",
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
				"services": {
					"type": "array",
					"items": {
						"type": "object",
						"properties": {
							"namespace": {"type": "string"},
							"name": {"type": "string"},
							"selector": {"type": "object", "additionalProperties": {"type": "string"}},
							"type": {"type": "string"},
							"ipFamilies": {"type": "array", "items": {"type": "string"}},
							"ipFamilyPolicy": {"type": "string"},
							"internalTrafficPolicy": {"type": "string"},
							"externalTrafficPolicy": {"type": "string"},
							"ports": {
								"type": "array",
								"items": {
									"type": "object",
									"properties": {
										"name": {"type": "string"},
										"protocol": {"type": "string"},
										"port": {"type": "integer"},
										"target_port": {"type": "string"},
										"node_port": {"type": "integer"}
									},
									"required": ["port"]
								}
							}
						},
						"required": ["namespace", "name", "type"]
					}
				}
			},
			"required": ["services"]
		}`),
		Annotations: readOnly(),
	}
}

// NewGetServicesHandler returns the kubectl_get_services handler.
func NewGetServicesHandler(c kube.Clients) mcp.ToolFunction {
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

		out := servicesOutput{Services: []serviceRow{}}
		var links []mcp.Content
		for _, ns := range namespacesOrAll(args.Namespaces) {
			list, err := cs.CoreV1().Services(ns).List(ctx, metav1.ListOptions{})
			if err != nil {
				return mcp.ErrorResultf("failed to list services in namespace %q on context %q: %v", ns, args.Context, err), nil
			}
			for _, svc := range list.Items {
				row := serviceRow{
					Namespace: svc.Namespace,
					Name:      svc.Name,
					Selector:  svc.Spec.Selector,
					Type:      string(svc.Spec.Type),
				}
				for _, fam := range svc.Spec.IPFamilies {
					row.IPFamilies = append(row.IPFamilies, string(fam))
				}
				if svc.Spec.IPFamilyPolicy != nil {
					row.IPFamilyPolicy = string(*svc.Spec.IPFamilyPolicy)
				}
				if svc.Spec.InternalTrafficPolicy != nil {
					row.InternalTrafficPolicy = string(*svc.Spec.InternalTrafficPolicy)
				}
				row.ExternalTrafficPolicy = string(svc.Spec.ExternalTrafficPolicy)
				for _, p := range svc.Spec.Ports {
					sp := servicePort{
						Name:     p.Name,
						Protocol: string(p.Protocol),
						Port:     p.Port,
						NodePort: p.NodePort,
					}
					if p.TargetPort.String() != "0" {
						sp.TargetPort = p.TargetPort.String()
					}
					row.Ports = append(row.Ports, sp)
				}
				out.Services = append(out.Services, row)
				links = append(links, mcp.ResourceLinkContent(
					serviceURI(args.Context, svc.Namespace, svc.Name),
					svc.Name, "Service manifest with EndpointSlices", "application/json"))
			}
		}
		return jsonResult(out, links)
	}
}
