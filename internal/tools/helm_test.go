package tools

import (
	"bytes"
	"compress/gzip"
	"encoding/base64"
	"fmt"
	"strings"
	"testing"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	"github.com/spirilis/mcp-kube-baker/internal/helm"
)

// helmSecret builds the storage record Helm's default driver writes, encoded
// the way Helm encodes it: JSON, gzip, base64.
func helmSecret(t *testing.T, namespace, name string, revision int, chart, version string) *corev1.Secret {
	t.Helper()
	doc := fmt.Sprintf(`{
		"name": %q, "namespace": %q, "version": %d,
		"info": {"status": "deployed", "last_deployed": "2026-08-01T10:00:00Z", "description": "Install complete"},
		"chart": {"metadata": {"name": %q, "version": %q, "appVersion": "9.9.9"}, "values": {"replicas": 1}},
		"config": {"replicas": 3}
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

func TestGetHelmInstalls(t *testing.T) {
	c := newFakeClients(
		helmSecret(t, "default", "redis", 1, "redis", "1.0.0"),
		helmSecret(t, "default", "redis", 2, "redis", "1.1.0"),
		helmSecret(t, "cert-manager", "cert-manager", 7, "cert-manager", "v1.14.4"),
	)
	res := callTool(t, NewGetHelmInstallsHandler(c), `{"context":"test"}`)
	if res.IsError {
		t.Fatalf("unexpected tool error: %+v", res.Content)
	}
	out := res.StructuredContent.(helmInstallsOutput)
	if len(out.Releases) != 2 {
		t.Fatalf("expected 2 releases, got %+v", out.Releases)
	}

	// Only the current revision of each release, `helm list` style.
	redis := out.Releases[1]
	if redis.Name != "redis" || redis.Revision != 2 {
		t.Errorf("expected redis revision 2, got %+v", redis)
	}
	if redis.Chart != "redis-1.1.0" || redis.ChartName != "redis" || redis.ChartVer != "1.1.0" {
		t.Errorf("unexpected chart fields: %+v", redis)
	}
	if redis.AppVersion != "9.9.9" || redis.Status != "deployed" || redis.Storage != "secret" {
		t.Errorf("unexpected release fields: %+v", redis)
	}
	if redis.Updated != "2026-08-01T10:00:00Z" {
		t.Errorf("updated = %q", redis.Updated)
	}

	// One values resource_link per release, and no more.
	var links []string
	for _, content := range res.Content {
		if content.Type == "resource_link" {
			links = append(links, content.URI)
		}
	}
	if len(links) != 2 {
		t.Fatalf("expected 2 resource links, got %v", links)
	}
	if links[1] != "mcp+kubectl://test/helm-values/default/redis" {
		t.Errorf("unexpected link %q", links[1])
	}
}

func TestGetHelmInstallsNamespaceScoped(t *testing.T) {
	c := newFakeClients(
		helmSecret(t, "default", "redis", 1, "redis", "1.0.0"),
		helmSecret(t, "cert-manager", "cert-manager", 1, "cert-manager", "v1.14.4"),
	)
	res := callTool(t, NewGetHelmInstallsHandler(c), `{"context":"test","namespace":"cert-manager"}`)
	if res.IsError {
		t.Fatalf("unexpected tool error: %+v", res.Content)
	}
	out := res.StructuredContent.(helmInstallsOutput)
	if len(out.Releases) != 1 || out.Releases[0].Name != "cert-manager" {
		t.Fatalf("expected only cert-manager, got %+v", out.Releases)
	}
}

// A cluster with no Helm on it answers with an empty list, not an error.
func TestGetHelmInstallsEmpty(t *testing.T) {
	c := newFakeClients()
	res := callTool(t, NewGetHelmInstallsHandler(c), `{"context":"test"}`)
	if res.IsError {
		t.Fatalf("unexpected tool error: %+v", res.Content)
	}
	out := res.StructuredContent.(helmInstallsOutput)
	if len(out.Releases) != 0 {
		t.Errorf("expected no releases, got %+v", out.Releases)
	}
	if !strings.Contains(contentText(t, res), `"releases": []`) {
		t.Errorf("empty result should render an empty array: %s", contentText(t, res))
	}
}

// An undecodable record is reported alongside the releases that did decode.
func TestGetHelmInstallsWarnsOnBrokenRecord(t *testing.T) {
	broken := &corev1.Secret{
		ObjectMeta: metav1.ObjectMeta{
			Namespace: "default",
			Name:      "sh.helm.release.v1.broken.v1",
			Labels:    map[string]string{helm.OwnerLabel: helm.OwnerHelm, helm.NameLabel: "broken"},
		},
		Type: helm.SecretType,
		Data: map[string][]byte{helm.ReleaseKey: []byte("!!! not base64 !!!")},
	}
	c := newFakeClients(broken, helmSecret(t, "default", "redis", 1, "redis", "1.0.0"))
	res := callTool(t, NewGetHelmInstallsHandler(c), `{"context":"test"}`)
	if res.IsError {
		t.Fatalf("unexpected tool error: %+v", res.Content)
	}
	out := res.StructuredContent.(helmInstallsOutput)
	if len(out.Releases) != 1 || out.Releases[0].Name != "redis" {
		t.Errorf("expected redis to survive the broken record, got %+v", out.Releases)
	}
	if len(out.Warnings) != 1 || !strings.Contains(out.Warnings[0], "broken") {
		t.Errorf("expected a warning naming the broken record, got %v", out.Warnings)
	}
}

func TestGetHelmInstallsUnknownContext(t *testing.T) {
	c := newFakeClients()
	res := callTool(t, NewGetHelmInstallsHandler(c), `{"context":"nope"}`)
	if !res.IsError {
		t.Fatal("expected IsError for unknown context")
	}
}
