package helm

import (
	"bytes"
	"compress/gzip"
	"encoding/base64"
	"encoding/json"
	"strings"
	"testing"
	"time"
)

// releaseJSON is the shape Helm writes: the fields this package declares plus
// several it does not, which must survive decoding by being ignored.
const releaseJSON = `{
  "name": "cert-manager",
  "namespace": "cert-manager",
  "version": 3,
  "info": {
    "first_deployed": "2026-01-02T03:04:05Z",
    "last_deployed": "2026-08-01T10:00:00Z",
    "deleted": "0001-01-01T00:00:00Z",
    "description": "Upgrade complete",
    "status": "deployed",
    "notes": "cert-manager is up"
  },
  "chart": {
    "metadata": {"name": "cert-manager", "version": "v1.14.4", "appVersion": "v1.14.4", "type": "application"},
    "values": {"installCRDs": false, "replicaCount": 1},
    "templates": [{"name": "templates/deployment.yaml", "data": "..."}]
  },
  "config": {"installCRDs": true},
  "manifest": "apiVersion: v1\nkind: Service\n",
  "hooks": [{"name": "startupapicheck"}]
}`

// encode reproduces Helm's storage encoding: JSON, gzip, base64.
func encode(t *testing.T, doc string) []byte {
	t.Helper()
	var buf bytes.Buffer
	zw, err := gzip.NewWriterLevel(&buf, gzip.BestCompression)
	if err != nil {
		t.Fatalf("gzip.NewWriterLevel: %v", err)
	}
	if _, err := zw.Write([]byte(doc)); err != nil {
		t.Fatalf("gzip write: %v", err)
	}
	if err := zw.Close(); err != nil {
		t.Fatalf("gzip close: %v", err)
	}
	return []byte(base64.StdEncoding.EncodeToString(buf.Bytes()))
}

func TestDecodeGzipped(t *testing.T) {
	rel, err := Decode(encode(t, releaseJSON))
	if err != nil {
		t.Fatalf("Decode: %v", err)
	}
	if rel.Name != "cert-manager" || rel.Namespace != "cert-manager" || rel.Version != 3 {
		t.Errorf("unexpected identity: %+v", rel)
	}
	if rel.Info == nil || rel.Info.Status != "deployed" || rel.Info.Description != "Upgrade complete" {
		t.Fatalf("unexpected info: %+v", rel.Info)
	}
	want := time.Date(2026, 8, 1, 10, 0, 0, 0, time.UTC)
	if !rel.Info.LastDeployed.Equal(want) {
		t.Errorf("last_deployed = %v, want %v", rel.Info.LastDeployed, want)
	}
	if rel.Chart == nil || rel.Chart.Metadata == nil || rel.Chart.Metadata.AppVersion != "v1.14.4" {
		t.Fatalf("unexpected chart: %+v", rel.Chart)
	}
	if got := rel.Chart.Values["replicaCount"]; got != float64(1) {
		t.Errorf("chart default replicaCount = %v", got)
	}
	if got := rel.Config["installCRDs"]; got != true {
		t.Errorf("user-supplied installCRDs = %v, want true", got)
	}
	if got := rel.ChartRef(); got != "cert-manager-v1.14.4" {
		t.Errorf("ChartRef() = %q", got)
	}
}

// Records written before Helm compressed them are plain base64 JSON, and the
// gzip magic check is what keeps those readable.
func TestDecodeUncompressed(t *testing.T) {
	rel, err := Decode([]byte(base64.StdEncoding.EncodeToString([]byte(releaseJSON))))
	if err != nil {
		t.Fatalf("Decode: %v", err)
	}
	if rel.Name != "cert-manager" || rel.Version != 3 {
		t.Errorf("unexpected release: %+v", rel)
	}
}

// A record whose "deleted" is empty or null must still decode: nothing here
// displays it, and Helm's own struct has carried it both ways.
func TestDecodeTolerantTimestamps(t *testing.T) {
	for _, deleted := range []string{`""`, `null`, `"0001-01-01T00:00:00Z"`} {
		doc := `{"name":"x","namespace":"y","version":1,"info":{"deleted":` + deleted + `,"status":"deployed"}}`
		rel, err := Decode(encode(t, doc))
		if err != nil {
			t.Fatalf("Decode with deleted=%s: %v", deleted, err)
		}
		if !rel.Info.Deleted.IsZero() {
			t.Errorf("deleted=%s decoded to non-zero %v", deleted, rel.Info.Deleted)
		}
	}
}

func TestDecodeErrors(t *testing.T) {
	if _, err := Decode([]byte("not base64 at all!!")); err == nil {
		t.Error("expected an error for non-base64 input")
	}
	notJSON := base64.StdEncoding.EncodeToString([]byte("{definitely not json"))
	if _, err := Decode([]byte(notJSON)); err == nil {
		t.Error("expected an error for non-JSON payload")
	}
	// Gzip magic with a truncated body: the reader must fail, not hang or panic.
	broken := base64.StdEncoding.EncodeToString([]byte{0x1f, 0x8b, 0x08, 0x00, 0x01})
	if _, err := Decode([]byte(broken)); err == nil {
		t.Error("expected an error for truncated gzip payload")
	}
}

func TestChartRef(t *testing.T) {
	cases := []struct {
		name string
		rel  Release
		want string
	}{
		{"no chart", Release{}, ""},
		{"no metadata", Release{Chart: &Chart{}}, ""},
		{"no name", Release{Chart: &Chart{Metadata: &Metadata{Version: "1.0.0"}}}, ""},
		{"no version", Release{Chart: &Chart{Metadata: &Metadata{Name: "redis"}}}, "redis"},
		{"both", Release{Chart: &Chart{Metadata: &Metadata{Name: "redis", Version: "1.0.0"}}}, "redis-1.0.0"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := tc.rel.ChartRef(); got != tc.want {
				t.Errorf("ChartRef() = %q, want %q", got, tc.want)
			}
		})
	}
}

// The decoder must not choke on a release whose values carry the deep, mixed
// structures real charts produce.
func TestDecodeNestedValues(t *testing.T) {
	doc := `{"name":"x","namespace":"y","version":1,"config":{"a":{"b":[1,"two",{"c":null}]}}}`
	rel, err := Decode(encode(t, doc))
	if err != nil {
		t.Fatalf("Decode: %v", err)
	}
	round, err := json.Marshal(rel.Config)
	if err != nil {
		t.Fatalf("Marshal: %v", err)
	}
	if !strings.Contains(string(round), `"two"`) {
		t.Errorf("nested values lost in decoding: %s", round)
	}
}
