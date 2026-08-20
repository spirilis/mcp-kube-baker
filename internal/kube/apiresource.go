package kube

import (
	"errors"
	"fmt"
	"strings"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/discovery"
)

// CoreGroupSentinel is the literal that stands in for the core (empty-named)
// API group wherever a group has to be written down — a URI path segment or a
// tool argument. The core group's real name is "", which neither surface can
// carry.
const CoreGroupSentinel = "core"

var (
	// ErrUnknownGroupVersion means the cluster does not serve the named API
	// group-version at all.
	ErrUnknownGroupVersion = errors.New("unknown api group-version")
	// ErrUnknownKind means the group-version is served but has no resource
	// answering to the given token.
	ErrUnknownKind = errors.New("unknown kind")
)

// GroupVersion renders a group/version pair the way the API server does: bare
// version for the core group, "group/version" otherwise.
func GroupVersion(group, version string) string {
	if group == "" {
		return version
	}
	return group + "/" + version
}

// NormalizeGroup maps the ways a caller can spell the core API group onto its
// real, empty name.
func NormalizeGroup(group string) string {
	if group == CoreGroupSentinel {
		return ""
	}
	return group
}

// ResolveAPIResource maps a Kind token onto the canonical APIResource the
// cluster serves for it. The token is accepted in any of the spellings a human
// would reach for — the Kind, the plural resource name, the singular name, or a
// short name — because a request composed from a kubectl-shaped mental model
// should still resolve. Subresources ("pods/status") are never candidates.
//
// Callers distinguish the two failures with errors.Is: ErrUnknownGroupVersion
// and ErrUnknownKind both mean "you named something outside the keyspace",
// which each surface reports in its own idiom.
func ResolveAPIResource(d discovery.DiscoveryInterface, group, version, token string) (*metav1.APIResource, error) {
	gv := GroupVersion(group, version)
	list, err := d.ServerResourcesForGroupVersion(gv)
	if err != nil {
		return nil, fmt.Errorf("%w %q: %v", ErrUnknownGroupVersion, gv, err)
	}
	for i := range list.APIResources {
		res := &list.APIResources[i]
		if strings.Contains(res.Name, "/") {
			continue
		}
		if matchesToken(res, token) {
			// Discovery omits group/version on the per-group-version list;
			// fill them in so a caller can build a GVR without carrying the
			// coordinates it asked with.
			out := *res
			out.Group, out.Version = group, version
			return &out, nil
		}
	}
	return nil, fmt.Errorf("%w %q in %q", ErrUnknownKind, token, gv)
}

func matchesToken(res *metav1.APIResource, token string) bool {
	if strings.EqualFold(res.Kind, token) ||
		strings.EqualFold(res.Name, token) ||
		(res.SingularName != "" && strings.EqualFold(res.SingularName, token)) {
		return true
	}
	for _, short := range res.ShortNames {
		if strings.EqualFold(short, token) {
			return true
		}
	}
	return false
}
