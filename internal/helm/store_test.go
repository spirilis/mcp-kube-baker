package helm

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"testing"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/kubernetes/fake"
)

// releaseSecret builds the Secret Helm's default storage driver writes.
func releaseSecret(t *testing.T, namespace, name string, revision int, status, chart, version string) *corev1.Secret {
	t.Helper()
	return &corev1.Secret{
		ObjectMeta: metav1.ObjectMeta{
			Namespace: namespace,
			Name:      fmt.Sprintf("sh.helm.release.v1.%s.v%d", name, revision),
			Labels: map[string]string{
				OwnerLabel:   OwnerHelm,
				NameLabel:    name,
				VersionLabel: fmt.Sprint(revision),
				"status":     status,
			},
		},
		Type: SecretType,
		Data: map[string][]byte{ReleaseKey: encode(t, releaseDoc(namespace, name, revision, status, chart, version))},
	}
}

// releaseConfigMap builds the equivalent record for HELM_DRIVER=configmap.
func releaseConfigMap(t *testing.T, namespace, name string, revision int, status, chart, version string) *corev1.ConfigMap {
	t.Helper()
	return &corev1.ConfigMap{
		ObjectMeta: metav1.ObjectMeta{
			Namespace: namespace,
			Name:      fmt.Sprintf("sh.helm.release.v1.%s.v%d", name, revision),
			Labels: map[string]string{
				OwnerLabel:   OwnerHelm,
				NameLabel:    name,
				VersionLabel: fmt.Sprint(revision),
				"status":     status,
			},
		},
		Data: map[string]string{ReleaseKey: string(encode(t, releaseDoc(namespace, name, revision, status, chart, version)))},
	}
}

func releaseDoc(namespace, name string, revision int, status, chart, version string) string {
	return fmt.Sprintf(`{
		"name": %q, "namespace": %q, "version": %d,
		"info": {"status": %q, "last_deployed": "2026-08-01T10:00:00Z", "description": "Install complete"},
		"chart": {"metadata": {"name": %q, "version": %q, "appVersion": "1.2.3"}, "values": {"replicas": 1}},
		"config": {"replicas": 3}
	}`, name, namespace, revision, status, chart, version)
}

func TestListLatestPicksHighestRevision(t *testing.T) {
	cs := fake.NewClientset(
		releaseSecret(t, "default", "redis", 1, "superseded", "redis", "1.0.0"),
		releaseSecret(t, "default", "redis", 2, "deployed", "redis", "1.1.0"),
		releaseSecret(t, "cert-manager", "cert-manager", 7, "deployed", "cert-manager", "v1.14.4"),
		releaseConfigMap(t, "legacy", "old-app", 4, "deployed", "old-app", "0.9.0"),
	)

	records, warnings, err := ListLatest(context.Background(), cs, metav1.NamespaceAll)
	if err != nil {
		t.Fatalf("ListLatest: %v", err)
	}
	if len(warnings) != 0 {
		t.Errorf("unexpected warnings: %v", warnings)
	}
	if len(records) != 3 {
		t.Fatalf("expected 3 releases, got %d: %+v", len(records), records)
	}
	// Sorted by namespace, then name.
	if records[0].Release.Name != "cert-manager" || records[1].Release.Name != "redis" || records[2].Release.Name != "old-app" {
		t.Errorf("unexpected ordering: %s %s %s", records[0].Release.Name, records[1].Release.Name, records[2].Release.Name)
	}
	redis := records[1]
	if redis.Release.Version != 2 || redis.Release.ChartRef() != "redis-1.1.0" {
		t.Errorf("expected redis revision 2 (redis-1.1.0), got %d (%s)", redis.Release.Version, redis.Release.ChartRef())
	}
	if redis.Storage != StorageSecret {
		t.Errorf("redis storage = %q", redis.Storage)
	}
	if records[2].Storage != StorageConfigMap {
		t.Errorf("configmap-driver release reported storage %q", records[2].Storage)
	}
}

func TestListLatestScopedToNamespace(t *testing.T) {
	cs := fake.NewClientset(
		releaseSecret(t, "default", "redis", 1, "deployed", "redis", "1.0.0"),
		releaseSecret(t, "other", "nginx", 1, "deployed", "nginx", "2.0.0"),
	)
	records, _, err := ListLatest(context.Background(), cs, "other")
	if err != nil {
		t.Fatalf("ListLatest: %v", err)
	}
	if len(records) != 1 || records[0].Release.Name != "nginx" {
		t.Fatalf("expected only nginx, got %+v", records)
	}
}

// Neither a Secret that merely wears owner=helm nor an undecodable record may
// take the whole listing down with it.
func TestListLatestSkipsForeignAndBrokenRecords(t *testing.T) {
	foreign := &corev1.Secret{
		ObjectMeta: metav1.ObjectMeta{Namespace: "default", Name: "not-helm", Labels: map[string]string{OwnerLabel: OwnerHelm}},
		Type:       corev1.SecretTypeOpaque,
		Data:       map[string][]byte{ReleaseKey: []byte("garbage")},
	}
	broken := &corev1.Secret{
		ObjectMeta: metav1.ObjectMeta{Namespace: "default", Name: "sh.helm.release.v1.broken.v1", Labels: map[string]string{OwnerLabel: OwnerHelm, NameLabel: "broken"}},
		Type:       SecretType,
		Data:       map[string][]byte{ReleaseKey: []byte("!!! not base64 !!!")},
	}
	// Right type, no owner label: the label selector is what has to exclude it.
	unlabelled := &corev1.Secret{
		ObjectMeta: metav1.ObjectMeta{Namespace: "default", Name: "sh.helm.release.v1.ghost.v1"},
		Type:       SecretType,
		Data:       map[string][]byte{ReleaseKey: []byte("!!! not base64 !!!")},
	}
	cs := fake.NewClientset(foreign, broken, unlabelled, releaseSecret(t, "default", "redis", 1, "deployed", "redis", "1.0.0"))

	records, warnings, err := ListLatest(context.Background(), cs, metav1.NamespaceAll)
	if err != nil {
		t.Fatalf("ListLatest: %v", err)
	}
	if len(records) != 1 || records[0].Release.Name != "redis" {
		t.Fatalf("expected only redis to survive, got %+v", records)
	}
	if len(warnings) != 1 || !strings.Contains(warnings[0], "sh.helm.release.v1.broken.v1") {
		t.Errorf("expected one warning naming the broken record, got %v", warnings)
	}
}

func TestLatest(t *testing.T) {
	cs := fake.NewClientset(
		releaseSecret(t, "default", "redis", 1, "superseded", "redis", "1.0.0"),
		releaseSecret(t, "default", "redis", 3, "deployed", "redis", "1.2.0"),
		releaseSecret(t, "default", "redis", 2, "superseded", "redis", "1.1.0"),
		releaseSecret(t, "default", "nginx", 9, "deployed", "nginx", "2.0.0"),
	)
	rec, err := Latest(context.Background(), cs, "default", "redis")
	if err != nil {
		t.Fatalf("Latest: %v", err)
	}
	if rec.Release.Version != 3 || rec.Release.ChartRef() != "redis-1.2.0" {
		t.Errorf("expected revision 3 (redis-1.2.0), got %d (%s)", rec.Release.Version, rec.Release.ChartRef())
	}
	if got := rec.Release.Config["replicas"]; got != float64(3) {
		t.Errorf("user-supplied replicas = %v, want 3", got)
	}

	if _, err := Latest(context.Background(), cs, "default", "absent"); !errors.Is(err, ErrNoRelease) {
		t.Errorf("expected ErrNoRelease for an unknown release, got %v", err)
	}
	if _, err := Latest(context.Background(), cs, "elsewhere", "redis"); !errors.Is(err, ErrNoRelease) {
		t.Errorf("expected ErrNoRelease for the wrong namespace, got %v", err)
	}
}

// The name label is written by whoever wrote the record; the release document
// is the authority on what it holds.
func TestLatestIgnoresMislabelledRecord(t *testing.T) {
	mislabelled := releaseSecret(t, "default", "redis", 5, "deployed", "redis", "9.9.9")
	mislabelled.Labels[NameLabel] = "redis"
	mislabelled.Data[ReleaseKey] = encode(t, releaseDoc("default", "something-else", 5, "deployed", "other", "9.9.9"))
	cs := fake.NewClientset(mislabelled, releaseSecret(t, "default", "redis", 1, "deployed", "redis", "1.0.0"))

	rec, err := Latest(context.Background(), cs, "default", "redis")
	if err != nil {
		t.Fatalf("Latest: %v", err)
	}
	if rec.Release.Version != 1 || rec.Release.Name != "redis" {
		t.Errorf("expected the genuine redis revision 1, got %s revision %d", rec.Release.Name, rec.Release.Version)
	}
}
