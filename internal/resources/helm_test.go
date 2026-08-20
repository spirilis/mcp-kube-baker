package resources

import (
	"bytes"
	"compress/gzip"
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"testing"

	"github.com/spirilis/generic-go-mcp/mcp"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	"github.com/spirilis/mcp-kube-baker/internal/helm"
)

// helmSecret builds a Helm storage record the way Helm's secrets driver does.
func helmSecret(t *testing.T, namespace, name string, revision int, chart, version string) *corev1.Secret {
	t.Helper()
	doc := fmt.Sprintf(`{
		"name": %q, "namespace": %q, "version": %d,
		"info": {"status": "deployed", "last_deployed": "2026-08-01T10:00:00Z", "description": "Upgrade complete"},
		"chart": {"metadata": {"name": %q, "version": %q, "appVersion": "9.9.9"}, "values": {"replicas": 1, "image": {"tag": "default"}}},
		"config": {"replicas": 3, "image": {"tag": "custom"}}
	}`, name, namespace, revision, chart, version)

	var buf bytes.Buffer
	zw, _ := gzip.NewWriterLevel(&buf, gzip.BestCompression)
	if _, err := zw.Write([]byte(doc)); err != nil {
		t.Fatalf("gzip write: %v", err)
	}
	if err := zw.Close(); err != nil {
		t.Fatalf("gzip close: %v", err)
	}

	return &corev1.Secret{
		ObjectMeta: metav1.ObjectMeta{
			Namespace: namespace,
			Name:      fmt.Sprintf("sh.helm.release.v1.%s.v%d", name, revision),
			Labels: map[string]string{
				helm.OwnerLabel:   helm.OwnerHelm,
				helm.NameLabel:    name,
				helm.VersionLabel: fmt.Sprint(revision),
			},
		},
		Type: helm.SecretType,
		Data: map[string][]byte{helm.ReleaseKey: []byte(base64.StdEncoding.EncodeToString(buf.Bytes()))},
	}
}

func TestReadHelmValues(t *testing.T) {
	rr, _ := newFixture(t,
		helmSecret(t, "default", "redis", 1, "redis", "1.0.0"),
		helmSecret(t, "default", "redis", 2, "redis", "1.1.0"),
	)
	res, err := read(t, rr, "mcp+kubectl://test/helm-values/default/redis")
	if err != nil {
		t.Fatalf("read: %v", err)
	}
	if res.MimeType != "application/json" {
		t.Errorf("unexpected mime type %q", res.MimeType)
	}

	var doc map[string]interface{}
	if err := json.Unmarshal([]byte(res.Text), &doc); err != nil {
		t.Fatalf("content is not valid JSON: %v", err)
	}
	// The current revision, not the first one.
	if doc["revision"] != float64(2) || doc["chart"] != "redis-1.1.0" {
		t.Errorf("expected revision 2 of redis-1.1.0, got revision %v of %v", doc["revision"], doc["chart"])
	}
	if doc["release"] != "redis" || doc["namespace"] != "default" || doc["storage"] != "secret" {
		t.Errorf("unexpected identity fields: %+v", doc)
	}
	if doc["status"] != "deployed" || doc["updated"] != "2026-08-01T10:00:00Z" || doc["app_version"] != "9.9.9" {
		t.Errorf("unexpected status fields: %+v", doc)
	}

	// User-supplied and chart-default values stay separate and unmerged.
	user := doc["user_supplied_values"].(map[string]interface{})
	if user["replicas"] != float64(3) || user["image"].(map[string]interface{})["tag"] != "custom" {
		t.Errorf("unexpected user-supplied values: %+v", user)
	}
	defaults := doc["chart_default_values"].(map[string]interface{})
	if defaults["replicas"] != float64(1) || defaults["image"].(map[string]interface{})["tag"] != "default" {
		t.Errorf("unexpected chart default values: %+v", defaults)
	}
}

// A release installed with no overrides must render an empty object, so a
// reader can tell it apart from a missing field.
func TestReadHelmValuesWithoutOverrides(t *testing.T) {
	bare := helmSecret(t, "default", "bare", 1, "bare", "1.0.0")
	var buf bytes.Buffer
	zw, _ := gzip.NewWriterLevel(&buf, gzip.BestCompression)
	if _, err := zw.Write([]byte(`{"name":"bare","namespace":"default","version":1,"chart":{"metadata":{"name":"bare","version":"1.0.0"}}}`)); err != nil {
		t.Fatalf("gzip write: %v", err)
	}
	if err := zw.Close(); err != nil {
		t.Fatalf("gzip close: %v", err)
	}
	bare.Data[helm.ReleaseKey] = []byte(base64.StdEncoding.EncodeToString(buf.Bytes()))

	rr, _ := newFixture(t, bare)
	res, err := read(t, rr, "mcp+kubectl://test/helm-values/default/bare")
	if err != nil {
		t.Fatalf("read: %v", err)
	}
	var doc map[string]interface{}
	if err := json.Unmarshal([]byte(res.Text), &doc); err != nil {
		t.Fatalf("content is not valid JSON: %v", err)
	}
	user, ok := doc["user_supplied_values"].(map[string]interface{})
	if !ok || len(user) != 0 {
		t.Errorf("expected an empty user_supplied_values object, got %#v", doc["user_supplied_values"])
	}
}

func TestReadHelmValuesNotFound(t *testing.T) {
	rr, _ := newFixture(t, helmSecret(t, "default", "redis", 1, "redis", "1.0.0"))
	for _, uri := range []string{
		"mcp+kubectl://test/helm-values/default/absent",
		"mcp+kubectl://test/helm-values/elsewhere/redis",
		"mcp+kubectl://nope/helm-values/default/redis",
	} {
		if _, err := read(t, rr, uri); !errors.Is(err, mcp.ErrResourceNotFound) {
			t.Errorf("%s: expected ErrResourceNotFound, got %v", uri, err)
		}
	}
}

func TestCompleteHelmRelease(t *testing.T) {
	_, c := newFixture(t,
		helmSecret(t, "default", "redis", 1, "redis", "1.0.0"),
		helmSecret(t, "default", "redis-ha", 1, "redis-ha", "1.0.0"),
		helmSecret(t, "default", "nginx", 1, "nginx", "2.0.0"),
	)
	completer := NewCompleter(c)
	complete := func(value string, vars map[string]string) []string {
		t.Helper()
		res, err := completer.Complete(context.Background(), &mcp.CompletionRequest{
			URITemplate: HelmValuesTemplate, Argument: "release", Value: value, Context: vars,
		})
		if err != nil {
			t.Fatalf("Complete: %v", err)
		}
		return res.Values
	}

	got := complete("", map[string]string{"context": "test", "namespace": "default"})
	if len(got) != 3 {
		t.Errorf("expected 3 releases, got %v", got)
	}
	got = complete("redis", map[string]string{"context": "test", "namespace": "default"})
	if len(got) != 2 || got[0] != "redis" || got[1] != "redis-ha" {
		t.Errorf("unexpected prefix-filtered completions: %v", got)
	}
	// Without a resolved namespace there is nothing to enumerate.
	if got = complete("", map[string]string{"context": "test"}); len(got) != 0 {
		t.Errorf("expected no completions without a namespace, got %v", got)
	}
	if got = complete("", map[string]string{"context": "nope", "namespace": "default"}); len(got) != 0 {
		t.Errorf("expected no completions for an unknown context, got %v", got)
	}
}
