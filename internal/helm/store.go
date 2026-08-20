package helm

import (
	"context"
	"errors"
	"fmt"
	"sort"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/kubernetes"
)

// ErrNoRelease means no Helm storage record names the release asked for.
var ErrNoRelease = errors.New("no such helm release")

// Record is one decoded storage record together with where it was found.
type Record struct {
	Release *Release
	// Storage is the driver that held it: "secret" or "configmap".
	Storage string
}

// Storage driver names as reported on a Record.
const (
	StorageSecret    = "secret"
	StorageConfigMap = "configmap"
)

// ListLatest returns the newest revision of every Helm release in one
// namespace, or across the cluster when namespace is empty (metav1.NamespaceAll).
//
// Both storage drivers are read: Helm defaults to Secrets, but HELM_DRIVER=configmap
// installs exist and are invisible if only Secrets are consulted. Records that
// fail to decode are reported as warnings instead of failing the whole listing —
// one malformed record should not hide every other release in the namespace.
func ListLatest(ctx context.Context, cs kubernetes.Interface, namespace string) ([]*Record, []string, error) {
	selector := metav1.ListOptions{LabelSelector: OwnerLabel + "=" + OwnerHelm}

	var records []*Record
	var warnings []string

	secrets, err := cs.CoreV1().Secrets(namespace).List(ctx, selector)
	if err != nil {
		return nil, nil, fmt.Errorf("failed to list helm release secrets: %w", err)
	}
	for i := range secrets.Items {
		s := &secrets.Items[i]
		if s.Type != SecretType {
			// Someone else's Secret wearing owner=helm is not a release record.
			continue
		}
		rel, err := decodeFrom(s.Data[ReleaseKey], s.Namespace, s.Name)
		if err != nil {
			warnings = append(warnings, err.Error())
			continue
		}
		records = append(records, &Record{Release: rel, Storage: StorageSecret})
	}

	configMaps, err := cs.CoreV1().ConfigMaps(namespace).List(ctx, selector)
	if err != nil {
		// A cluster may legitimately deny configmap reads while allowing
		// secrets; that should cost the configmap driver, not the listing.
		warnings = append(warnings, fmt.Sprintf("failed to list helm release configmaps: %v", err))
	} else {
		for i := range configMaps.Items {
			cm := &configMaps.Items[i]
			rel, err := decodeFrom([]byte(cm.Data[ReleaseKey]), cm.Namespace, cm.Name)
			if err != nil {
				warnings = append(warnings, err.Error())
				continue
			}
			records = append(records, &Record{Release: rel, Storage: StorageConfigMap})
		}
	}

	return latestPerRelease(records), warnings, nil
}

// Latest returns the newest revision of one named release in one namespace.
func Latest(ctx context.Context, cs kubernetes.Interface, namespace, release string) (*Record, error) {
	selector := metav1.ListOptions{
		LabelSelector: OwnerLabel + "=" + OwnerHelm + "," + NameLabel + "=" + release,
	}

	var records []*Record
	secrets, err := cs.CoreV1().Secrets(namespace).List(ctx, selector)
	if err != nil {
		return nil, fmt.Errorf("failed to list helm release secrets: %w", err)
	}
	for i := range secrets.Items {
		s := &secrets.Items[i]
		if s.Type != SecretType {
			continue
		}
		rel, err := decodeFrom(s.Data[ReleaseKey], s.Namespace, s.Name)
		if err != nil {
			return nil, err
		}
		records = append(records, &Record{Release: rel, Storage: StorageSecret})
	}
	configMaps, err := cs.CoreV1().ConfigMaps(namespace).List(ctx, selector)
	if err == nil {
		for i := range configMaps.Items {
			cm := &configMaps.Items[i]
			rel, err := decodeFrom([]byte(cm.Data[ReleaseKey]), cm.Namespace, cm.Name)
			if err != nil {
				return nil, err
			}
			records = append(records, &Record{Release: rel, Storage: StorageConfigMap})
		}
	}

	// The label carries the release name, but the record itself is the
	// authority on it; a mislabelled record must not answer for a name it does
	// not hold.
	var best *Record
	for _, rec := range records {
		if rec.Release.Name != release {
			continue
		}
		if best == nil || rec.Release.Version > best.Release.Version {
			best = rec
		}
	}
	if best == nil {
		return nil, fmt.Errorf("%w: %s/%s", ErrNoRelease, namespace, release)
	}
	return best, nil
}

// decodeFrom decodes one record's payload, naming the object it came from if
// that fails.
func decodeFrom(data []byte, namespace, name string) (*Release, error) {
	if len(data) == 0 {
		return nil, fmt.Errorf("helm storage record %s/%s has no %q key", namespace, name, ReleaseKey)
	}
	rel, err := Decode(data)
	if err != nil {
		return nil, fmt.Errorf("helm storage record %s/%s: %w", namespace, name, err)
	}
	if rel.Namespace == "" {
		// Pre-3.2 records left this off; the holding object still knows.
		rel.Namespace = namespace
	}
	return rel, nil
}

// latestPerRelease collapses every revision of a release to its highest one —
// what `helm list` shows — and sorts the result by namespace then name.
func latestPerRelease(records []*Record) []*Record {
	type key struct{ namespace, name string }
	best := map[key]*Record{}
	for _, rec := range records {
		k := key{rec.Release.Namespace, rec.Release.Name}
		if cur, ok := best[k]; !ok || rec.Release.Version > cur.Release.Version {
			best[k] = rec
		}
	}
	out := make([]*Record, 0, len(best))
	for _, rec := range best {
		out = append(out, rec)
	}
	sort.Slice(out, func(i, j int) bool {
		if out[i].Release.Namespace != out[j].Release.Namespace {
			return out[i].Release.Namespace < out[j].Release.Namespace
		}
		return out[i].Release.Name < out[j].Release.Name
	})
	return out
}
