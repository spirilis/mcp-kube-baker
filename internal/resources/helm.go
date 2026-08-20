package resources

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/spirilis/generic-go-mcp/mcp"

	"github.com/spirilis/mcp-kube-baker/internal/helm"
	"github.com/spirilis/mcp-kube-baker/internal/kube"
)

// HelmValuesTemplate addresses the values of one Helm release's current
// revision.
const HelmValuesTemplate = "mcp+kubectl://{context}/helm-values/{namespace}/{release}"

// readHelmValues serves `helm get values` for a release: the values the user
// supplied at install/upgrade time.
//
// The chart's own defaults ship alongside them rather than merged into them.
// Helm's coalescing rules are subtler than a deep merge — subchart scoping,
// null-means-delete, global values — so a merge computed here would be a
// plausible-looking lie in exactly the cases that matter. Two labelled objects
// let the reader see which value came from where, which is what `helm get
// values` and `helm get values --all` show side by side.
func readHelmValues(c kube.Clients) mcp.ResourceTemplateFunction {
	return func(ctx context.Context, req *mcp.ResourceReadRequest) (mcp.ResourceContentResult, error) {
		cs, err := clientFor(c, req.Vars["context"])
		if err != nil {
			return mcp.ResourceContentResult{}, err
		}
		ctx, cancel := context.WithTimeout(ctx, kube.APITimeout)
		defer cancel()

		ns, name := req.Vars["namespace"], req.Vars["release"]
		rec, err := helm.Latest(ctx, cs, ns, name)
		if err != nil {
			if errors.Is(err, helm.ErrNoRelease) {
				return mcp.ResourceContentResult{}, fmt.Errorf("helm release %s/%s: %w", ns, name, mcp.ErrResourceNotFound)
			}
			return mcp.ResourceContentResult{}, fmt.Errorf("failed to read helm release %s/%s: %w", ns, name, err)
		}

		rel := rec.Release
		doc := map[string]interface{}{
			"release":   rel.Name,
			"namespace": rel.Namespace,
			"revision":  rel.Version,
			"storage":   rec.Storage,
			// `helm get values` — what the user passed, and nothing else.
			"user_supplied_values": orEmpty(rel.Config),
		}
		if rel.Chart != nil {
			doc["chart_default_values"] = orEmpty(rel.Chart.Values)
			if rel.Chart.Metadata != nil {
				doc["chart"] = rel.ChartRef()
				doc["chart_name"] = rel.Chart.Metadata.Name
				doc["chart_version"] = rel.Chart.Metadata.Version
				doc["app_version"] = rel.Chart.Metadata.AppVersion
			}
		}
		if rel.Info != nil {
			doc["status"] = rel.Info.Status
			doc["description"] = rel.Info.Description
			if !rel.Info.LastDeployed.IsZero() {
				doc["updated"] = rel.Info.LastDeployed.Format(time.RFC3339)
			}
		}
		return jsonContent(doc)
	}
}

// orEmpty keeps a values object present-but-empty rather than null, so a reader
// can tell "installed with no overrides" from "field missing".
func orEmpty(v map[string]interface{}) map[string]interface{} {
	if v == nil {
		return map[string]interface{}{}
	}
	return v
}
