package health

import (
	"net/http"
	"net/http/httptest"
	"testing"
)

// newMux returns a mux carrying only the probe routes — the same thing the HTTP
// transport's ExtraRoutes hook builds, minus the listener.
func newMux(t *testing.T) *http.ServeMux {
	t.Helper()
	mux := http.NewServeMux()
	Routes(mux)
	return mux
}

func TestProbesRespond(t *testing.T) {
	tests := []struct {
		path string
		want string
	}{
		{"/healthz", `{"status":"healthy"}`},
		{"/readyz", `{"status":"ready"}`},
	}
	mux := newMux(t)
	for _, tt := range tests {
		t.Run(tt.path, func(t *testing.T) {
			rec := httptest.NewRecorder()
			mux.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, tt.path, nil))

			if rec.Code != http.StatusOK {
				t.Fatalf("status = %d, want %d", rec.Code, http.StatusOK)
			}
			if got := rec.Body.String(); got != tt.want {
				t.Errorf("body = %q, want %q", got, tt.want)
			}
			if got := rec.Header().Get("Content-Type"); got != "application/json" {
				t.Errorf("Content-Type = %q, want application/json", got)
			}
		})
	}
}

// A prober sends GET (or HEAD, which the GET pattern also matches). Anything
// else is not a probe, and the method pattern must say so rather than answering
// "healthy" to it.
func TestNonGETIsRejected(t *testing.T) {
	rec := httptest.NewRecorder()
	newMux(t).ServeHTTP(rec, httptest.NewRequest(http.MethodPost, "/healthz", nil))

	if rec.Code != http.StatusMethodNotAllowed {
		t.Errorf("status = %d, want %d", rec.Code, http.StatusMethodNotAllowed)
	}
}

// Everything outside the probe paths stays a plain 404: this hook adds two
// endpoints, it does not take over routing.
func TestUnrelatedPathIsNotFound(t *testing.T) {
	rec := httptest.NewRecorder()
	newMux(t).ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/nope", nil))

	if rec.Code != http.StatusNotFound {
		t.Errorf("status = %d, want %d", rec.Code, http.StatusNotFound)
	}
}

// ExtraRoutes runs before the transport registers /mcp and before an
// AuthService registers its own routes, and http.ServeMux panics on a duplicate
// pattern. So a probe path that ever grew into one of those would take the
// server down at startup. Registering them all here after Routes reproduces
// that ordering: the test panics — and fails — if a collision is ever
// introduced.
func TestProbesDoNotClaimProtocolOrAuthPaths(t *testing.T) {
	mux := newMux(t)
	nop := func(http.ResponseWriter, *http.Request) {}

	for _, pattern := range []string{
		"/mcp",
		"/authorize", "/token", "/callback", "/register",
		"/admin/", "/.well-known/",
	} {
		mux.HandleFunc(pattern, nop) // panics on collision
	}
}
