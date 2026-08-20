package tools

import (
	"context"
	"encoding/json"
	"time"

	"github.com/spirilis/generic-go-mcp/mcp"

	"github.com/spirilis/mcp-kube-baker/internal/helm"
	"github.com/spirilis/mcp-kube-baker/internal/kube"
)

type helmReleaseRow struct {
	Namespace   string `json:"namespace"`
	Name        string `json:"name"`
	Revision    int    `json:"revision"`
	Status      string `json:"status,omitempty"`
	Chart       string `json:"chart,omitempty"`
	ChartName   string `json:"chart_name,omitempty"`
	ChartVer    string `json:"chart_version,omitempty"`
	AppVersion  string `json:"app_version,omitempty"`
	Updated     string `json:"updated,omitempty"`
	Description string `json:"description,omitempty"`
	Storage     string `json:"storage"`
}

type helmInstallsOutput struct {
	Releases []helmReleaseRow `json:"releases"`
	Warnings []string         `json:"warnings,omitempty"`
}

// GetHelmInstallsDefinition describes the kubectl_get_helm_installs tool.
func GetHelmInstallsDefinition() mcp.Tool {
	return mcp.Tool{
		Name:        "kubectl_get_helm_installs",
		Title:       "List Helm releases",
		Description: "Returns the current revision of every Helm release on one Kubernetes cluster — release name, chart and chart version, app version, status, and when it was last deployed — the way `helm list` does. Omit namespace for all namespaces. The values a release was installed with are available as an mcp+kubectl://{context}/helm-values/{namespace}/{release} resource.",
		InputSchema: json.RawMessage(`{
			"type": "object",
			"properties": {
				"context": {"type": "string", "description": "Kubeconfig context naming the cluster (see kubectl_get_contexts)"},
				"namespace": {"type": "string", "description": "Optional namespace; omit for releases in all namespaces"}
			},
			"required": ["context"]
		}`),
		OutputSchema: json.RawMessage(`{
			"type": "object",
			"properties": {
				"releases": {
					"type": "array",
					"items": {
						"type": "object",
						"properties": {
							"namespace": {"type": "string"},
							"name": {"type": "string"},
							"revision": {"type": "integer"},
							"status": {"type": "string"},
							"chart": {"type": "string", "description": "chart name and version, e.g. cert-manager-v1.14.4"},
							"chart_name": {"type": "string"},
							"chart_version": {"type": "string"},
							"app_version": {"type": "string"},
							"updated": {"type": "string", "description": "RFC 3339 timestamp of the last deployment"},
							"description": {"type": "string"},
							"storage": {"type": "string", "description": "Helm storage driver the record came from: secret or configmap"}
						},
						"required": ["namespace", "name", "revision", "storage"]
					}
				},
				"warnings": {"type": "array", "items": {"type": "string"}}
			},
			"required": ["releases"]
		}`),
		Annotations: readOnly(),
	}
}

// NewGetHelmInstallsHandler returns the kubectl_get_helm_installs handler.
func NewGetHelmInstallsHandler(c kube.Clients) mcp.ToolFunction {
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

		// An empty namespace is metav1.NamespaceAll, which is exactly the
		// "omit for all namespaces" contract of the argument.
		records, warnings, err := helm.ListLatest(ctx, cs, args.Namespace)
		if err != nil {
			return mcp.ErrorResultf("failed to list helm releases on context %q: %v", args.Context, err), nil
		}

		out := helmInstallsOutput{Releases: []helmReleaseRow{}, Warnings: warnings}
		var links []mcp.Content
		for _, rec := range records {
			rel := rec.Release
			row := helmReleaseRow{
				Namespace: rel.Namespace,
				Name:      rel.Name,
				Revision:  rel.Version,
				Chart:     rel.ChartRef(),
				Storage:   rec.Storage,
			}
			if rel.Info != nil {
				row.Status = rel.Info.Status
				row.Description = rel.Info.Description
				if !rel.Info.LastDeployed.IsZero() {
					row.Updated = rel.Info.LastDeployed.Format(time.RFC3339)
				}
			}
			if rel.Chart != nil && rel.Chart.Metadata != nil {
				row.ChartName = rel.Chart.Metadata.Name
				row.ChartVer = rel.Chart.Metadata.Version
				row.AppVersion = rel.Chart.Metadata.AppVersion
			}
			out.Releases = append(out.Releases, row)
			links = append(links, mcp.ResourceLinkContent(
				helmValuesURI(args.Context, rel.Namespace, rel.Name),
				rel.Name, "Values of Helm release "+rel.Name, "application/json"))
		}
		return jsonResult(out, links)
	}
}
