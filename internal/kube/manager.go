// Package kube wraps client-go for mcp-kube-baker: it enumerates the contexts
// of one external kubeconfig file and hands out a lazily-built, cached
// clientset per context. All cluster selection in this server happens by
// kubeconfig context name.
package kube

import (
	"errors"
	"fmt"
	"sort"
	"sync"
	"time"

	"k8s.io/client-go/dynamic"
	"k8s.io/client-go/kubernetes"
	"k8s.io/client-go/openapi"
	"k8s.io/client-go/openapi/cached"
	"k8s.io/client-go/rest"
	"k8s.io/client-go/tools/clientcmd"
	api "k8s.io/client-go/tools/clientcmd/api"
)

// APITimeout bounds every Kubernetes API call made on behalf of one MCP
// request. Derive per-call contexts from the request context with this.
const APITimeout = 30 * time.Second

// ErrUnknownContext is returned by Client for a context name that does not
// exist in the kubeconfig. Callers translate it to a model-recoverable tool
// error or a resource not-found, depending on surface.
var ErrUnknownContext = errors.New("unknown kubeconfig context")

// Clients is the surface tools and resources consume. Tests substitute a fake
// backed by k8s.io/client-go/kubernetes/fake clientsets.
type Clients interface {
	// Contexts returns all context names (sorted) and the current-context.
	Contexts() (names []string, current string)
	// Client returns the clientset for one context, building and caching it
	// on first use. Unknown context names return ErrUnknownContext.
	Client(contextName string) (kubernetes.Interface, error)
	// Dynamic returns the cluster's dynamic (unstructured) client, used to
	// fetch Kinds this server has no compiled-in Go type for — CRDs above
	// all. Unknown context names return ErrUnknownContext.
	Dynamic(contextName string) (dynamic.Interface, error)
	// OpenAPI returns the cluster's OpenAPI V3 client. Group-version schema
	// documents run to hundreds of kilobytes, so the returned client memoizes
	// them: that is not an optimization here, it is what keeps a second read of
	// the same group-version from refetching the whole document.
	OpenAPI(contextName string) (openapi.Client, error)
}

// ClientManager implements Clients against a kubeconfig file loaded once at
// startup.
type ClientManager struct {
	apiConfig *api.Config

	mu       sync.Mutex
	clients  map[string]kubernetes.Interface
	dynamics map[string]dynamic.Interface
	openapis map[string]openapi.Client
}

// NewClientManager loads the kubeconfig at path and validates it has at least
// one context. No cluster is contacted here — clientsets are built lazily.
func NewClientManager(path string) (*ClientManager, error) {
	apiConfig, err := clientcmd.LoadFromFile(path)
	if err != nil {
		return nil, fmt.Errorf("failed to load kubeconfig %q: %w", path, err)
	}
	if len(apiConfig.Contexts) == 0 {
		return nil, fmt.Errorf("kubeconfig %q defines no contexts", path)
	}
	return &ClientManager{
		apiConfig: apiConfig,
		clients:   make(map[string]kubernetes.Interface),
		dynamics:  make(map[string]dynamic.Interface),
		openapis:  make(map[string]openapi.Client),
	}, nil
}

// Contexts implements Clients.
func (m *ClientManager) Contexts() ([]string, string) {
	names := make([]string, 0, len(m.apiConfig.Contexts))
	for name := range m.apiConfig.Contexts {
		names = append(names, name)
	}
	sort.Strings(names)
	return names, m.apiConfig.CurrentContext
}

// Client implements Clients.
func (m *ClientManager) Client(contextName string) (kubernetes.Interface, error) {
	if _, ok := m.apiConfig.Contexts[contextName]; !ok {
		return nil, fmt.Errorf("%w: %q", ErrUnknownContext, contextName)
	}

	m.mu.Lock()
	defer m.mu.Unlock()
	if cs, ok := m.clients[contextName]; ok {
		return cs, nil
	}

	restConfig, err := m.restConfig(contextName)
	if err != nil {
		return nil, err
	}
	cs, err := kubernetes.NewForConfig(restConfig)
	if err != nil {
		return nil, fmt.Errorf("failed to build clientset for context %q: %w", contextName, err)
	}
	m.clients[contextName] = cs
	return cs, nil
}

// restConfig builds the rest.Config for one context. Callers hold m.mu.
func (m *ClientManager) restConfig(contextName string) (*rest.Config, error) {
	cfg, err := clientcmd.NewNonInteractiveClientConfig(
		*m.apiConfig, contextName, &clientcmd.ConfigOverrides{}, nil,
	).ClientConfig()
	if err != nil {
		return nil, fmt.Errorf("failed to build client config for context %q: %w", contextName, err)
	}
	return cfg, nil
}

// Dynamic implements Clients.
func (m *ClientManager) Dynamic(contextName string) (dynamic.Interface, error) {
	if _, ok := m.apiConfig.Contexts[contextName]; !ok {
		return nil, fmt.Errorf("%w: %q", ErrUnknownContext, contextName)
	}

	m.mu.Lock()
	defer m.mu.Unlock()
	if dc, ok := m.dynamics[contextName]; ok {
		return dc, nil
	}

	restConfig, err := m.restConfig(contextName)
	if err != nil {
		return nil, err
	}
	dc, err := dynamic.NewForConfig(restConfig)
	if err != nil {
		return nil, fmt.Errorf("failed to build dynamic client for context %q: %w", contextName, err)
	}
	m.dynamics[contextName] = dc
	return dc, nil
}

// OpenAPI implements Clients.
func (m *ClientManager) OpenAPI(contextName string) (openapi.Client, error) {
	// Resolve through Client so an unknown context still reports
	// ErrUnknownContext rather than a nil-client panic downstream.
	cs, err := m.Client(contextName)
	if err != nil {
		return nil, err
	}

	m.mu.Lock()
	defer m.mu.Unlock()
	if oc, ok := m.openapis[contextName]; ok {
		return oc, nil
	}
	oc := cached.NewClient(cs.Discovery().OpenAPIV3())
	m.openapis[contextName] = oc
	return oc, nil
}
