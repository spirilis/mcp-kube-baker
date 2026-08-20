package resources

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"sort"
	"strings"

	"github.com/spirilis/generic-go-mcp/mcp"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/discovery"

	"github.com/spirilis/mcp-kube-baker/internal/kube"
)

// The api-resource templates. Two shapes exist because the core API group's
// name is the empty string, and {var} compiles to a matcher that never accepts
// an empty path segment — "mcp+kubectl://ctx/api-resource//v1/Pod" could never
// match anything. So core is addressable either by the CoreGroupSentinel or by
// the short form that omits the group entirely. The two differ in segment
// count, so the registry's first-match-wins scan can never confuse them.
const (
	APIResourceTemplate     = "mcp+kubectl://{context}/api-resource/{apiGroup}/{version}/{kind}"
	CoreAPIResourceTemplate = "mcp+kubectl://{context}/api-resource/{version}/{kind}"
)

// CoreGroupSentinel is the literal that stands in for the core (empty-named)
// API group in an api-resource URI. It is the same token the arbitrary-manifest
// tool takes, so both surfaces spell core the one way.
const CoreGroupSentinel = kube.CoreGroupSentinel

// schemaRefPrefix is the only $ref form an OpenAPI V3 group-version document
// produced by a Kubernetes API server uses.
const schemaRefPrefix = "#/components/schemas/"

// readAPIResource serves both api-resource templates: it resolves {kind} to a
// canonical Kind through discovery, then extracts that Kind's schema and every
// schema it transitively references out of the group-version's OpenAPI V3
// document.
//
// Returning the whole group-version document instead would be simpler and four
// times larger (apps/v1 is ~750KB against ~177KB for Deployment's closure),
// and most of it would be about Kinds the URI did not name.
func readAPIResource(c kube.Clients) mcp.ResourceTemplateFunction {
	return func(ctx context.Context, req *mcp.ResourceReadRequest) (mcp.ResourceContentResult, error) {
		cs, err := clientFor(c, req.Vars["context"])
		if err != nil {
			return mcp.ResourceContentResult{}, err
		}

		// Absent on the short template; the sentinel on the long one. Both mean
		// the core group.
		group := req.Vars["apiGroup"]
		if group == CoreGroupSentinel {
			group = ""
		}
		version, kind := req.Vars["version"], req.Vars["kind"]

		// Discovery and OpenAPI calls take no context.Context — client-go bounds
		// them with the rest client's own timeout rather than a caller's.
		res, err := resolveAPIResource(cs.Discovery(), group, version, kind)
		if err != nil {
			return mcp.ResourceContentResult{}, err
		}

		oc, err := c.OpenAPI(req.Vars["context"])
		if err != nil {
			return mcp.ResourceContentResult{}, fmt.Errorf("failed to build OpenAPI client: %w", err)
		}
		paths, err := oc.Paths()
		if err != nil {
			return mcp.ResourceContentResult{}, fmt.Errorf("failed to list OpenAPI paths: %w", err)
		}
		path := openAPIPath(group, version)
		gv, ok := paths[path]
		if !ok {
			return mcp.ResourceContentResult{}, fmt.Errorf("no OpenAPI V3 document at %q: %w", path, mcp.ErrResourceNotFound)
		}
		raw, err := gv.Schema("application/json")
		if err != nil {
			return mcp.ResourceContentResult{}, fmt.Errorf("failed to fetch OpenAPI V3 document %q: %w", path, err)
		}

		root, closure, err := schemaClosure(raw, group, version, res.Kind)
		if err != nil {
			return mcp.ResourceContentResult{}, err
		}

		doc := map[string]interface{}{
			"openapi": "3.0.0",
			"info": map[string]interface{}{
				"title":   fmt.Sprintf("%s %s", kube.GroupVersion(group, version), res.Kind),
				"version": version,
			},
			"components": map[string]interface{}{"schemas": closure},
			// Without this the reader has a bag of sibling components and no
			// statement of which one the URI actually named.
			"x-kubernetes-root-schema": root,
			"x-kubernetes-api-resource": map[string]interface{}{
				"group":      group,
				"version":    version,
				"kind":       res.Kind,
				"resource":   res.Name,
				"namespaced": res.Namespaced,
				"shortNames": res.ShortNames,
				"verbs":      []string(res.Verbs),
			},
		}
		return jsonContent(doc)
	}
}

// openAPIPath is the key a group-version has in the /openapi/v3 discovery map.
func openAPIPath(group, version string) string {
	if group == "" {
		return "api/" + version
	}
	return "apis/" + group + "/" + version
}

// resolveAPIResource maps the {kind} URI token onto a canonical APIResource,
// restating the shared resolver's two failures as resource-not-found: a URI
// naming an unserved group-version or an unknown Kind names something outside
// the keyspace, not a broken server.
func resolveAPIResource(d discovery.DiscoveryInterface, group, version, token string) (*metav1.APIResource, error) {
	res, err := kube.ResolveAPIResource(d, group, version, token)
	if err != nil {
		if errors.Is(err, kube.ErrUnknownGroupVersion) || errors.Is(err, kube.ErrUnknownKind) {
			return nil, fmt.Errorf("%v: %w", err, mcp.ErrResourceNotFound)
		}
		return nil, err
	}
	return res, nil
}

// schemaClosure finds the component schema for one group/version/kind in an
// OpenAPI V3 group-version document and returns it together with every schema
// reachable from it by $ref, keyed exactly as in the source document so the
// refs inside the closure stay resolvable without rewriting.
func schemaClosure(raw []byte, group, version, kind string) (string, map[string]interface{}, error) {
	var doc struct {
		Components struct {
			Schemas map[string]interface{} `json:"schemas"`
		} `json:"components"`
	}
	if err := json.Unmarshal(raw, &doc); err != nil {
		return "", nil, fmt.Errorf("failed to parse OpenAPI V3 document for %s: %w", kube.GroupVersion(group, version), err)
	}

	root := findSchemaByGVK(doc.Components.Schemas, group, version, kind)
	if root == "" {
		return "", nil, fmt.Errorf("no schema for %s %s: %w", kube.GroupVersion(group, version), kind, mcp.ErrResourceNotFound)
	}

	closure := map[string]interface{}{}
	frontier := []string{root}
	for len(frontier) > 0 {
		name := frontier[len(frontier)-1]
		frontier = frontier[:len(frontier)-1]
		if _, seen := closure[name]; seen {
			continue
		}
		schema, ok := doc.Components.Schemas[name]
		if !ok {
			continue // dangling $ref: the document's problem, not worth failing over
		}
		closure[name] = schema
		frontier = append(frontier, collectRefs(schema)...)
	}
	return root, closure, nil
}

// findSchemaByGVK locates the component whose x-kubernetes-group-version-kind
// extension claims this GVK. That extension is a list — shared kinds such as
// DeleteOptions and WatchEvent carry dozens of entries — so every entry is
// checked rather than just the first.
func findSchemaByGVK(schemas map[string]interface{}, group, version, kind string) string {
	// Sorted so a document that (wrongly) claims one GVK from two components
	// still resolves to the same one on every read.
	names := make([]string, 0, len(schemas))
	for name := range schemas {
		names = append(names, name)
	}
	sort.Strings(names)

	for _, name := range names {
		obj, ok := schemas[name].(map[string]interface{})
		if !ok {
			continue
		}
		gvks, ok := obj["x-kubernetes-group-version-kind"].([]interface{})
		if !ok {
			continue
		}
		for _, entry := range gvks {
			gvk, ok := entry.(map[string]interface{})
			if !ok {
				continue
			}
			if str(gvk["group"]) == group && str(gvk["version"]) == version && str(gvk["kind"]) == kind {
				return name
			}
		}
	}
	return ""
}

func str(v interface{}) string {
	s, _ := v.(string)
	return s
}

// collectRefs walks one decoded schema and reports the component names of every
// "#/components/schemas/..." $ref found anywhere beneath it.
func collectRefs(node interface{}) []string {
	var out []string
	switch n := node.(type) {
	case map[string]interface{}:
		for key, val := range n {
			if key == "$ref" {
				if ref, ok := val.(string); ok && strings.HasPrefix(ref, schemaRefPrefix) {
					out = append(out, strings.TrimPrefix(ref, schemaRefPrefix))
				}
				continue
			}
			out = append(out, collectRefs(val)...)
		}
	case []interface{}:
		for _, val := range n {
			out = append(out, collectRefs(val)...)
		}
	}
	return out
}
