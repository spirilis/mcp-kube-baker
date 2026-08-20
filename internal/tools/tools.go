// Package tools implements mcp-kube-baker's MCP tools. Every tool is
// read-only, takes a kubeconfig context name to select the cluster, and
// returns both human-readable JSON text Content and machine-readable
// StructuredContent validated against the tool's OutputSchema.
package tools

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/url"

	"github.com/spirilis/generic-go-mcp/mcp"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/kubernetes"

	"github.com/spirilis/mcp-kube-baker/internal/kube"
)

// maxResourceLinks caps the ResourceLink content entries appended to a tool
// result — a cluster with thousands of pods should not produce thousands of
// content blocks on top of the JSON that already lists them.
const maxResourceLinks = 100

// RegisterAll registers every tool against the given registry, bound to the
// given cluster client source.
func RegisterAll(reg *mcp.ToolRegistry, c kube.Clients) {
	reg.Register(GetContextsDefinition(), NewGetContextsHandler(c))
	reg.Register(GetNamespacesDefinition(), NewGetNamespacesHandler(c))
	reg.Register(GetPodsDefinition(), NewGetPodsHandler(c))
	reg.Register(GetPodLogsDefinition(), NewGetPodLogsHandler(c))
	reg.Register(GetEventsDefinition(), NewGetEventsHandler(c))
	reg.Register(GetNodesDefinition(), NewGetNodesHandler(c))
	reg.Register(GetServicesDefinition(), NewGetServicesHandler(c))
	reg.Register(GetAPIResourcesDefinition(), NewGetAPIResourcesHandler(c))
	reg.Register(GetHelmInstallsDefinition(), NewGetHelmInstallsHandler(c))
	reg.Register(GetArbitraryManifestDefinition(), NewGetArbitraryManifestHandler(c))
}

// readOnly is shared by every tool definition — nothing here mutates a
// cluster.
func readOnly() *mcp.ToolAnnotations {
	return &mcp.ToolAnnotations{ReadOnlyHint: true, IdempotentHint: true}
}

// jsonResult builds the standard tool result: pretty-printed JSON as text
// Content for the model, the same value as StructuredContent, plus optional
// ResourceLink entries (capped at maxResourceLinks).
func jsonResult(v interface{}, links []mcp.Content) (mcp.Result, error) {
	b, err := json.MarshalIndent(v, "", "  ")
	if err != nil {
		return nil, fmt.Errorf("failed to marshal tool output: %w", err)
	}
	content := []mcp.Content{mcp.Text(string(b))}
	if len(links) > maxResourceLinks {
		links = links[:maxResourceLinks]
	}
	content = append(content, links...)
	return &mcp.ToolCallResult{Content: content, StructuredContent: v}, nil
}

// clientFor resolves a context argument to a clientset, translating the
// errors a model can act on into tool errors (nil Go error).
func clientFor(c kube.Clients, contextName string) (kubernetes.Interface, *mcp.ToolCallResult) {
	if contextName == "" {
		return nil, mcp.ErrorResultf("the context argument is required: use kubectl_get_contexts to list available contexts")
	}
	cs, err := c.Client(contextName)
	if err != nil {
		if errors.Is(err, kube.ErrUnknownContext) {
			return nil, mcp.ErrorResultf("unknown context %q: use kubectl_get_contexts to list available contexts", contextName)
		}
		return nil, mcp.ErrorResultf("failed to build client for context %q: %v", contextName, err)
	}
	return cs, nil
}

// apiCtx derives the bounded context every Kubernetes API call runs under.
func apiCtx(ctx context.Context) (context.Context, context.CancelFunc) {
	return context.WithTimeout(ctx, kube.APITimeout)
}

// namespacesOrAll normalizes an optional namespaces argument: an empty list
// means one all-namespaces query.
func namespacesOrAll(namespaces []string) []string {
	if len(namespaces) == 0 {
		return []string{metav1.NamespaceAll}
	}
	return namespaces
}

// podURI etc. build the mcp+kubectl:// resource URIs matching the templates
// registered by internal/resources. Variables are percent-encoded the same
// way an RFC 6570 {var} expansion would encode them.
func podURI(kubeContext, namespace, name string) string {
	return fmt.Sprintf("mcp+kubectl://%s/pod/%s/%s", url.PathEscape(kubeContext), url.PathEscape(namespace), url.PathEscape(name))
}

func eventURI(kubeContext, namespace, name string) string {
	return fmt.Sprintf("mcp+kubectl://%s/event/%s/%s", url.PathEscape(kubeContext), url.PathEscape(namespace), url.PathEscape(name))
}

func nodeURI(kubeContext, name string) string {
	return fmt.Sprintf("mcp+kubectl://%s/node/%s", url.PathEscape(kubeContext), url.PathEscape(name))
}

func serviceURI(kubeContext, namespace, name string) string {
	return fmt.Sprintf("mcp+kubectl://%s/service/%s/%s", url.PathEscape(kubeContext), url.PathEscape(namespace), url.PathEscape(name))
}

func helmValuesURI(kubeContext, namespace, release string) string {
	return fmt.Sprintf("mcp+kubectl://%s/helm-values/%s/%s", url.PathEscape(kubeContext), url.PathEscape(namespace), url.PathEscape(release))
}

func apiResourceURI(kubeContext, apiGroup, version, kind string) string {
	if apiGroup == "" {
		// The core group's real name is the empty string, which is not a thing
		// a URI path segment can be — see internal/resources/apiresources.go.
		apiGroup = kube.CoreGroupSentinel
	}
	return fmt.Sprintf("mcp+kubectl://%s/api-resource/%s/%s/%s",
		url.PathEscape(kubeContext), url.PathEscape(apiGroup), url.PathEscape(version), url.PathEscape(kind))
}
