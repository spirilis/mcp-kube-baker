package tools

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"
	"time"

	"github.com/spirilis/generic-go-mcp/mcp"
	coordinationv1 "k8s.io/api/coordination/v1"
	corev1 "k8s.io/api/core/v1"
	k8serrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/fields"
	"k8s.io/client-go/kubernetes"

	"github.com/spirilis/mcp-kube-baker/internal/kube"
)

// The karpenter plugin. See plugins.go for how a plugin is registered; this
// file is only its tools.

const (
	// karpenterLeaseName is the leader-election Lease Karpenter creates through
	// controller-runtime.
	karpenterLeaseName = "karpenter-leader-election"
	// karpenterLeaseNamespace is where that Lease lives by default, and the
	// default for this tool's namespace argument.
	karpenterLeaseNamespace = "kube-system"
	// karpenterContainerName is Karpenter's own container, preferred over any
	// sidecar when the caller names no container.
	karpenterContainerName = "controller"
)

type karpenterLogsOutput struct {
	Context   string `json:"context"`
	LeaseName string `json:"lease_name"`
	// LeaseNamespace is echoed separately from the argument: a fuzzy lease match
	// must never be invisible to the caller.
	LeaseNamespace       string `json:"lease_namespace"`
	HolderIdentity       string `json:"holder_identity"`
	RenewTime            string `json:"renew_time,omitempty"`
	LeaseDurationSeconds *int32 `json:"lease_duration_seconds,omitempty"`
	Stale                bool   `json:"stale"`
	StaleReason          string `json:"stale_reason,omitempty"`
	Pod                  string `json:"pod"`
	PodNamespace         string `json:"pod_namespace"`
	Container            string `json:"container"`
	Previous             bool   `json:"previous"`
	Timestamps           bool   `json:"timestamps"`
	TailLines            *int64 `json:"tail_lines,omitempty"`
	MaxSizeKiB           *int   `json:"max_size_kib,omitempty"`
	Truncated            bool   `json:"truncated"`
	BytesReturned        int    `json:"bytes_returned"`
	BytesStreamed        int64  `json:"bytes_streamed"`
	Logs                 string `json:"logs"`
}

// KarpenterLogsDefinition describes the kubectl_karpenter_logs tool.
func KarpenterLogsDefinition() mcp.Tool {
	return mcp.Tool{
		Name:  "kubectl_karpenter_logs",
		Title: "Read the active Karpenter's logs",
		Description: "Returns the logs of the Karpenter replica that currently holds leadership — the only one " +
			"doing any provisioning, and therefore the only one whose logs explain why nodes are or are not " +
			"appearing. The active pod is found through Karpenter's leader-election Lease rather than guessed " +
			"at, so a multi-replica install needs no pod name. The window works exactly as in " +
			"kubectl_get_pod_logs: tail_lines bounds it by lines, max_size_kib by bytes, and giving neither " +
			"means the last 256 KiB.",
		InputSchema: json.RawMessage(`{
			"type": "object",
			"properties": {
				"context": {"type": "string", "description": "Kubeconfig context naming the cluster (see kubectl_get_contexts)"},
				"namespace": {"type": "string", "description": "Namespace holding Karpenter's leader-election Lease. Defaults to kube-system, which is where Karpenter puts it; set this only for an install that placed it elsewhere."},
				"container": {"type": "string", "description": "Container name; needed only if the leader pod has several containers and none is named 'controller'."},
				"previous": {"type": "boolean", "description": "Read the previously terminated container of the SAME pod — what 'kubectl logs -p' shows after a restart, useful when the leader was OOM-killed mid-provisioning. This is not the previous leader, which would be a different pod. Default false."},
				"tail_lines": {"type": "integer", "minimum": 1, "description": "Ask the API for only the last N lines. Omit for the whole retained log, subject to max_size_kib."},
				"max_size_kib": {"type": "integer", "minimum": 1, "maximum": 8192, "description": "Keep only the last N KiB of what was read. Defaults to 256 when tail_lines is also omitted; otherwise no byte limit is applied."},
				"timestamps": {"type": "boolean", "description": "Prefix each line with the kubelet's RFC3339Nano timestamp, which is what makes these lines correlatable with kubectl_get_events. Default true."}
			},
			"required": ["context"]
		}`),
		OutputSchema: json.RawMessage(`{
			"type": "object",
			"properties": {
				"context": {"type": "string"},
				"lease_name": {"type": "string", "description": "The leader-election Lease this resolved through"},
				"lease_namespace": {"type": "string"},
				"holder_identity": {"type": "string", "description": "The Lease's holderIdentity, which names the active pod"},
				"renew_time": {"type": "string"},
				"lease_duration_seconds": {"type": "integer"},
				"stale": {"type": "boolean", "description": "True when the Lease expired: the pod is the last known leader, not necessarily the current one"},
				"stale_reason": {"type": "string"},
				"pod": {"type": "string"},
				"pod_namespace": {"type": "string"},
				"container": {"type": "string"},
				"previous": {"type": "boolean"},
				"timestamps": {"type": "boolean"},
				"tail_lines": {"type": "integer"},
				"max_size_kib": {"type": "integer"},
				"truncated": {"type": "boolean", "description": "True when the window dropped the front of the log"},
				"bytes_returned": {"type": "integer"},
				"bytes_streamed": {"type": "integer", "description": "Bytes read from the API before the window was applied"},
				"logs": {"type": "string"}
			},
			"required": ["context", "lease_name", "lease_namespace", "holder_identity", "stale", "pod", "pod_namespace", "container", "previous", "timestamps", "truncated", "bytes_returned", "bytes_streamed", "logs"]
		}`),
		Annotations: readOnly(),
	}
}

// NewKarpenterLogsHandler returns the kubectl_karpenter_logs handler.
func NewKarpenterLogsHandler(c kube.Clients) mcp.ToolFunction {
	return func(ctx context.Context, req *mcp.ToolRequest) (mcp.Result, error) {
		var args struct {
			Context    string `json:"context"`
			Namespace  string `json:"namespace"`
			Container  string `json:"container"`
			Previous   bool   `json:"previous"`
			TailLines  *int64 `json:"tail_lines"`
			MaxSizeKiB *int   `json:"max_size_kib"`
			// A pointer: an omitted timestamps field must be distinguishable
			// from an explicit false, since the default is true.
			Timestamps *bool `json:"timestamps"`
		}
		if err := req.BindArguments(&args); err != nil {
			return mcp.ErrorResultf("invalid arguments: %v", err), nil
		}
		namespace := args.Namespace
		if namespace == "" {
			namespace = karpenterLeaseNamespace
		}
		window, errResult := resolveLogWindow(args.TailLines, args.MaxSizeKiB, args.Previous, args.Timestamps)
		if errResult != nil {
			return errResult, nil
		}
		cs, errResult := clientFor(c, args.Context)
		if errResult != nil {
			return errResult, nil
		}

		ctx, cancel := apiCtx(ctx)
		defer cancel()

		lease, errResult := findKarpenterLease(ctx, cs, args.Context, namespace)
		if errResult != nil {
			return errResult, nil
		}
		holder := ""
		if lease.Spec.HolderIdentity != nil {
			holder = *lease.Spec.HolderIdentity
		}
		if holder == "" {
			return mcp.ErrorResultf("the %s/%s Lease exists but names no holder, so no Karpenter replica currently "+
				"holds leadership — expected briefly during a rolling restart, and for as long as every replica is "+
				"down. Retry, or use kubectl_get_pods to check whether Karpenter is running at all.",
				lease.Namespace, lease.Name), nil
		}

		pod, errResult := karpenterLeaderPod(ctx, cs, args.Context, lease, holder)
		if errResult != nil {
			return errResult, nil
		}
		container, errResult := pickKarpenterContainer(pod, args.Container)
		if errResult != nil {
			return errResult, nil
		}
		fetched, errResult := fetchPodLogs(ctx, cs, args.Context, pod.Namespace, pod.Name, container, window)
		if errResult != nil {
			return errResult, nil
		}

		stale, staleReason := leaseStaleness(lease, time.Now())
		out := karpenterLogsOutput{
			Context:              args.Context,
			LeaseName:            lease.Name,
			LeaseNamespace:       lease.Namespace,
			HolderIdentity:       holder,
			LeaseDurationSeconds: lease.Spec.LeaseDurationSeconds,
			Stale:                stale,
			StaleReason:          staleReason,
			Pod:                  pod.Name,
			PodNamespace:         pod.Namespace,
			Container:            container,
			Previous:             window.Previous,
			Timestamps:           window.Timestamps,
			TailLines:            window.TailLines,
			MaxSizeKiB:           window.MaxSizeKiB,
			Truncated:            fetched.Truncated,
			BytesReturned:        fetched.BytesReturned,
			BytesStreamed:        fetched.BytesStreamed,
			Logs:                 string(fetched.Logs),
		}
		if lease.Spec.RenewTime != nil {
			out.RenewTime = lease.Spec.RenewTime.UTC().Format(time.RFC3339Nano)
		}

		// A staleness note leads the text, ahead of logText's own truncation
		// banner: whether these logs came from the current leader changes how to
		// read every line below it.
		var banners []string
		if staleReason != "" {
			banners = append(banners, "[lease "+lease.Namespace+"/"+lease.Name+": "+staleReason+"]")
		}

		return &mcp.ToolCallResult{
			Content: []mcp.Content{
				mcp.Text(logText(fetched, pod.Namespace, pod.Name, container, banners...)),
				mcp.ResourceLinkContent(podURI(args.Context, pod.Namespace, pod.Name),
					pod.Name, "Full Pod manifest of the Karpenter leader", "application/json"),
			},
			StructuredContent: out,
		}, nil
	}
}

// findKarpenterLease resolves Karpenter's leader-election Lease in namespace:
// the well-known name first, since it is fixed, then any Lease there whose name
// mentions karpenter, which covers forks and renamed installs. The chosen
// Lease's name is reported back to the caller, so a fuzzy match is never silent.
func findKarpenterLease(ctx context.Context, cs kubernetes.Interface, kubeContext, namespace string) (*coordinationv1.Lease, *mcp.ToolCallResult) {
	lease, err := cs.CoordinationV1().Leases(namespace).Get(ctx, karpenterLeaseName, metav1.GetOptions{})
	if err == nil {
		return lease, nil
	}
	if k8serrors.IsForbidden(err) {
		return nil, leaseForbidden(kubeContext, namespace, err)
	}
	if !k8serrors.IsNotFound(err) {
		return nil, mcp.ErrorResultf("failed to get the %s/%s Lease on context %q: %v",
			namespace, karpenterLeaseName, kubeContext, err)
	}

	list, err := cs.CoordinationV1().Leases(namespace).List(ctx, metav1.ListOptions{})
	if err != nil {
		if k8serrors.IsForbidden(err) {
			return nil, leaseForbidden(kubeContext, namespace, err)
		}
		return nil, mcp.ErrorResultf("failed to list Leases in namespace %q on context %q while looking for Karpenter's: %v",
			namespace, kubeContext, err)
	}

	var candidates []*coordinationv1.Lease
	for i := range list.Items {
		if strings.Contains(strings.ToLower(list.Items[i].Name), "karpenter") {
			candidates = append(candidates, &list.Items[i])
		}
	}
	switch len(candidates) {
	case 1:
		return candidates[0], nil
	case 0:
		return nil, mcp.ErrorResultf("namespace %q on context %q has no Lease named %q, and none whose name mentions "+
			"karpenter, so Karpenter does not appear to be installed there. If it runs in another namespace, pass "+
			"that as the namespace argument.", namespace, kubeContext, karpenterLeaseName)
	default:
		names := make([]string, len(candidates))
		for i, l := range candidates {
			names[i] = l.Name
		}
		return nil, mcp.ErrorResultf("namespace %q on context %q has no Lease named %q but %d whose names mention "+
			"karpenter, so the active pod is ambiguous: [%s]. Read the one you want with "+
			"kubectl_get_arbitrary_manifest to find its holderIdentity, then kubectl_get_pod_logs on that pod.",
			namespace, kubeContext, karpenterLeaseName, len(candidates), strings.Join(names, " "))
	}
}

// leaseForbidden carries the one RBAC message both Lease lookups need.
func leaseForbidden(kubeContext, namespace string, err error) *mcp.ToolCallResult {
	return mcp.ErrorResultf("not permitted to read Leases in namespace %q on context %q: finding the active "+
		"Karpenter pod needs get and list on coordination.k8s.io/v1 leases, which this server's credentials "+
		"lack: %v", namespace, kubeContext, err)
}

// karpenterLeaderPod resolves a Lease holderIdentity to the pod holding it.
func karpenterLeaderPod(ctx context.Context, cs kubernetes.Interface, kubeContext string, lease *coordinationv1.Lease, holder string) (*corev1.Pod, *mcp.ToolCallResult) {
	// controller-runtime builds the leader-election identity as
	// hostname + "_" + uuid, and a pod's hostname is its name. "_" is not legal
	// in a pod name, so the first one is an unambiguous split. An identity with
	// no "_" is used whole rather than rejected — some leader-election setups
	// omit the suffix, and if the result is not a pod name the lookup below says
	// so more usefully than a parse error would.
	podName, _, _ := strings.Cut(holder, "_")

	pod, err := cs.CoreV1().Pods(lease.Namespace).Get(ctx, podName, metav1.GetOptions{})
	if err == nil {
		return pod, nil
	}
	if !k8serrors.IsNotFound(err) {
		return nil, mcp.ErrorResultf("failed to fetch pod %s/%s, named by the %s/%s Lease holder %q, on context %q: %v",
			lease.Namespace, podName, lease.Namespace, lease.Name, holder, kubeContext, err)
	}

	// A Lease and the pod holding it need not share a namespace: an install can
	// set a leader-election namespace of its own. Search the cluster by name
	// before concluding the leader is gone.
	list, err := cs.CoreV1().Pods(metav1.NamespaceAll).List(ctx, metav1.ListOptions{
		FieldSelector: fields.OneTermEqualSelector("metadata.name", podName).String(),
	})
	if err != nil {
		return nil, mcp.ErrorResultf("failed to search every namespace for pod %q, named by the %s/%s Lease holder %q, on context %q: %v",
			podName, lease.Namespace, lease.Name, holder, kubeContext, err)
	}

	// The name is re-checked here rather than trusted: the field selector is
	// applied by the API server, and anything that ignored it would otherwise
	// hand back an arbitrary pod that this code would then read logs from.
	var found []*corev1.Pod
	for i := range list.Items {
		if list.Items[i].Name == podName {
			found = append(found, &list.Items[i])
		}
	}
	switch len(found) {
	case 1:
		return found[0], nil
	case 0:
		return nil, mcp.ErrorResultf("the %s/%s Lease names holder %q, but no pod called %q exists in any namespace "+
			"of context %q — the leader was most likely replaced just now. Retry, or use kubectl_get_pods to find "+
			"the current Karpenter pod.", lease.Namespace, lease.Name, holder, podName, kubeContext)
	default:
		namespaces := make([]string, len(found))
		for i, p := range found {
			namespaces[i] = p.Namespace
		}
		return nil, mcp.ErrorResultf("the %s/%s Lease names holder %q, but pods called %q exist in %d namespaces "+
			"[%s] on context %q, so which one holds the Lease is ambiguous. Read the one you want with "+
			"kubectl_get_pod_logs, naming its namespace explicitly.",
			lease.Namespace, lease.Name, holder, podName, len(found), strings.Join(namespaces, " "), kubeContext)
	}
}

// leaseStaleness reports whether the Lease has expired, and why, so the caller
// learns that these logs may be a former leader's rather than being quietly
// misled. An expired Lease is not an error: a Karpenter that lost or never
// renewed leadership is usually exactly what is being investigated.
//
// The empty reason means the Lease is current. A reason with stale=false means
// freshness could not be established at all.
func leaseStaleness(lease *coordinationv1.Lease, now time.Time) (bool, string) {
	if lease.Spec.RenewTime == nil || lease.Spec.LeaseDurationSeconds == nil {
		return false, "freshness unknown: the Lease sets no renewTime or leaseDurationSeconds, so whether this pod is still the leader cannot be told from it"
	}
	duration := time.Duration(*lease.Spec.LeaseDurationSeconds) * time.Second
	expiry := lease.Spec.RenewTime.Add(duration)
	if !now.After(expiry) {
		return false, ""
	}
	return true, fmt.Sprintf("EXPIRED %s ago: last renewed %s with a %s duration, so this pod is the last known "+
		"leader rather than certainly the current one",
		now.Sub(expiry).Round(time.Second), lease.Spec.RenewTime.UTC().Format(time.RFC3339), duration)
}

// pickKarpenterContainer chooses which container of the leader pod to read.
// Karpenter's own container is named "controller", and preferring it by name
// keeps a no-argument call working if a build ever adds a sidecar.
func pickKarpenterContainer(pod *corev1.Pod, want string) (string, *mcp.ToolCallResult) {
	main := containerNames(pod.Spec.Containers)
	others := containerNames(pod.Spec.InitContainers)
	for _, ec := range pod.Spec.EphemeralContainers {
		others = append(others, ec.Name)
	}

	if want != "" {
		for _, name := range append(append([]string{}, main...), others...) {
			if name == want {
				return want, nil
			}
		}
		return "", mcp.ErrorResultf("the Karpenter leader pod %s/%s has no container %q: %s",
			pod.Namespace, pod.Name, want, describeContainers(main, others))
	}
	for _, name := range main {
		if name == karpenterContainerName {
			return name, nil
		}
	}
	if len(main) == 1 {
		return main[0], nil
	}
	return "", mcp.ErrorResultf("the Karpenter leader pod %s/%s has %d containers and none named %q, so the "+
		"container argument is required: %s",
		pod.Namespace, pod.Name, len(main), karpenterContainerName, describeContainers(main, others))
}
