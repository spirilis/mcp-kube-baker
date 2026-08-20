// Package helm reads Helm 3 release records straight out of the cluster.
//
// Helm keeps one record per release revision in a Secret (the default storage
// driver since Helm 3) or a ConfigMap, labelled owner=helm, holding the release
// JSON gzipped and base64-encoded under the "release" key. Decoding that here
// costs about a hundred lines; depending on helm.sh/helm/v3 to do it would pull
// the entire Helm CLI — chart rendering, repository handling, its own
// Kubernetes client stack — into a read-only MCP server. The record format is
// the storage contract Helm itself must keep stable across versions, so this is
// the narrow surface to bind to.
package helm

import (
	"bytes"
	"compress/gzip"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"io"
	"time"
)

// OwnerLabel and its value select Helm's own storage records, whichever driver
// wrote them. Every list this package drives is scoped by it.
const (
	OwnerLabel = "owner"
	OwnerHelm  = "helm"

	// NameLabel and VersionLabel carry the release name and revision number.
	NameLabel    = "name"
	VersionLabel = "version"

	// ReleaseKey is the Secret/ConfigMap data key holding the encoded release.
	ReleaseKey = "release"

	// SecretType is the type Helm stamps on its release Secrets.
	SecretType = "helm.sh/release.v1"
)

// magicGzip is the gzip header Helm's encoder writes. Records predating
// compression are stored as plain JSON, so its absence is not an error.
var magicGzip = []byte{0x1f, 0x8b, 0x08}

// Release is the subset of Helm's release record this server surfaces. Helm's
// own struct carries the rendered manifest, every hook, and the chart's full
// file list; none of that belongs in a tool result, and JSON decoding drops
// what is not declared here.
type Release struct {
	Name      string                 `json:"name"`
	Namespace string                 `json:"namespace"`
	Version   int                    `json:"version"`
	Info      *Info                  `json:"info,omitempty"`
	Chart     *Chart                 `json:"chart,omitempty"`
	Config    map[string]interface{} `json:"config,omitempty"`
}

// Info is the release's status block.
type Info struct {
	FirstDeployed Time   `json:"first_deployed,omitempty"`
	LastDeployed  Time   `json:"last_deployed,omitempty"`
	Deleted       Time   `json:"deleted,omitempty"`
	Description   string `json:"description,omitempty"`
	Status        string `json:"status,omitempty"`
	Notes         string `json:"notes,omitempty"`
}

// Time is a timestamp that tolerates the empty string.
//
// Helm marshals an unset time as "0001-01-01T00:00:00Z", but records written by
// other tooling — and by Helm versions whose release struct differed — carry
// "" or null for a never-deleted release. encoding/json's time.Time rejects
// those outright, which would throw away an otherwise perfectly readable
// release over a field nothing here displays.
type Time struct{ time.Time }

// UnmarshalJSON implements json.Unmarshaler.
func (t *Time) UnmarshalJSON(b []byte) error {
	if string(b) == "null" || string(b) == `""` {
		t.Time = time.Time{}
		return nil
	}
	return t.Time.UnmarshalJSON(b)
}

// Chart is the installed chart, reduced to its metadata and default values.
type Chart struct {
	Metadata *Metadata              `json:"metadata,omitempty"`
	Values   map[string]interface{} `json:"values,omitempty"`
}

// Metadata is the chart's Chart.yaml.
type Metadata struct {
	Name        string   `json:"name,omitempty"`
	Version     string   `json:"version,omitempty"`
	AppVersion  string   `json:"appVersion,omitempty"`
	Description string   `json:"description,omitempty"`
	Home        string   `json:"home,omitempty"`
	Sources     []string `json:"sources,omitempty"`
	Deprecated  bool     `json:"deprecated,omitempty"`
	Type        string   `json:"type,omitempty"`
}

// Decode turns one storage record's "release" value into a Release. data is
// the raw value as client-go hands it over — for a Secret that is the already
// base64-decoded []byte, for a ConfigMap the string, both of which are still
// Helm's own base64 of (optionally gzipped) JSON.
func Decode(data []byte) (*Release, error) {
	raw, err := base64.StdEncoding.DecodeString(string(data))
	if err != nil {
		return nil, fmt.Errorf("release record is not base64: %w", err)
	}
	if len(raw) > 3 && bytes.Equal(raw[:3], magicGzip) {
		zr, err := gzip.NewReader(bytes.NewReader(raw))
		if err != nil {
			return nil, fmt.Errorf("failed to open gzip reader on release record: %w", err)
		}
		defer zr.Close()
		// Helm writes these; a release big enough to matter would already have
		// failed the 1MB Secret limit on the way in.
		raw, err = io.ReadAll(zr)
		if err != nil {
			return nil, fmt.Errorf("failed to decompress release record: %w", err)
		}
	}
	var rel Release
	if err := json.Unmarshal(raw, &rel); err != nil {
		return nil, fmt.Errorf("failed to parse release record JSON: %w", err)
	}
	return &rel, nil
}

// ChartRef renders the "chart-version" string helm list prints, e.g.
// "cert-manager-v1.14.4". An empty chart name yields an empty ref rather than a
// dangling dash.
func (r *Release) ChartRef() string {
	if r.Chart == nil || r.Chart.Metadata == nil || r.Chart.Metadata.Name == "" {
		return ""
	}
	if r.Chart.Metadata.Version == "" {
		return r.Chart.Metadata.Name
	}
	return r.Chart.Metadata.Name + "-" + r.Chart.Metadata.Version
}
