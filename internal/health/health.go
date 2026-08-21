// Package health serves mcp-kube-baker's liveness and readiness probes on the
// same listener as /mcp, for a Kubernetes Deployment's httpGet probes or a
// Docker HEALTHCHECK. The HTTP transport owns its own mux and http.Server, so
// generic-go-mcp v0.8.1's HTTPTransportConfig.ExtraRoutes hook is the only seam
// these can be mounted on.
package health

import "net/http"

// Routes registers the probe endpoints on the transport's mux. Its signature is
// exactly transport.HTTPTransportConfig.ExtraRoutes, so main assigns it by name.
//
// Both probes answer from local state alone: no cluster is contacted. The
// kubeconfig is validated once at startup and clientsets are built lazily per
// context, so there is nothing remote to wait on — and dialing an API server
// here would be actively wrong for a deliberately multi-cluster server, since
// one unreachable cluster would pull the pod out of its Service while
// kubectl_get_contexts and every other context still answer correctly.
// Liveness and readiness stay separate paths because a deployment expects them
// to be, not because the answers can diverge: the process is either serving or
// it is not.
//
// These routes sit outside the auth middleware, which wraps /mcp alone — a
// probe carries no token. That is why the payloads name no context, cluster, or
// anything else about the environment.
//
// The method patterns (Go 1.22+) make a non-GET a 405 rather than a 200; GET
// also matches HEAD, which is what a HEALTHCHECK using curl -I sends.
func Routes(mux *http.ServeMux) {
	mux.HandleFunc("GET /healthz", respondJSON(`{"status":"healthy"}`))
	mux.HandleFunc("GET /readyz", respondJSON(`{"status":"ready"}`))
}

// respondJSON builds a handler that writes one fixed JSON body. A write error
// means the prober hung up, which is nothing this process can act on.
func respondJSON(body string) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.Write([]byte(body))
	}
}
