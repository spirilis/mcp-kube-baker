package tools

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"strings"

	"github.com/spirilis/generic-go-mcp/mcp"
	corev1 "k8s.io/api/core/v1"
	k8serrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/kubernetes"

	"github.com/spirilis/mcp-kube-baker/internal/kube"
)

const (
	// defaultMaxSizeKiB is the tail window applied when the caller bounds
	// neither the lines nor the bytes.
	defaultMaxSizeKiB = 256
	// maxMaxSizeKiB ceilings max_size_kib. The retained bytes are held in
	// memory, so the argument must not be able to ask for an arbitrary
	// allocation.
	maxMaxSizeKiB = 8192
)

type podLogsOutput struct {
	Context    string `json:"context"`
	Namespace  string `json:"namespace"`
	Pod        string `json:"pod"`
	Container  string `json:"container"`
	Previous   bool   `json:"previous"`
	Timestamps bool   `json:"timestamps"`
	// TailLines and MaxSizeKiB echo the window that was actually applied,
	// including the default one the caller did not ask for.
	TailLines     *int64 `json:"tail_lines,omitempty"`
	MaxSizeKiB    *int   `json:"max_size_kib,omitempty"`
	Truncated     bool   `json:"truncated"`
	BytesReturned int    `json:"bytes_returned"`
	BytesStreamed int64  `json:"bytes_streamed"`
	Logs          string `json:"logs"`
}

// GetPodLogsDefinition describes the kubectl_get_pod_logs tool.
func GetPodLogsDefinition() mcp.Tool {
	return mcp.Tool{
		Name:        "kubectl_get_pod_logs",
		Title:       "Read container logs",
		Description: "Returns the tail of one container's stdout/stderr logs, the way `kubectl logs` does. Container names come from kubectl_get_pods. The window is yours to choose: tail_lines bounds it by lines, max_size_kib by bytes, both together apply the byte window to the last tail_lines lines, and neither means the last 256 KiB.",
		InputSchema: json.RawMessage(`{
			"type": "object",
			"properties": {
				"context": {"type": "string", "description": "Kubeconfig context naming the cluster (see kubectl_get_contexts)"},
				"namespace": {"type": "string", "description": "Namespace holding the pod"},
				"pod": {"type": "string", "description": "Pod name (see kubectl_get_pods)"},
				"container": {"type": "string", "description": "Container name; may be omitted only when the pod has exactly one container. Init and ephemeral containers are accepted too."},
				"previous": {"type": "boolean", "description": "Read the previously terminated container instead of the running one — what 'kubectl logs -p' shows after a restart. Default false."},
				"tail_lines": {"type": "integer", "minimum": 1, "description": "Ask the API for only the last N lines. Omit for the whole retained log."},
				"max_size_kib": {"type": "integer", "minimum": 1, "maximum": 8192, "description": "Keep only the last N KiB of what was read. Defaults to 256 when tail_lines is also omitted; otherwise no byte limit is applied."},
				"timestamps": {"type": "boolean", "description": "Prefix each line with the kubelet's RFC3339Nano timestamp, which is what makes these lines correlatable with kubectl_get_events. Default true."}
			},
			"required": ["context", "namespace", "pod"]
		}`),
		OutputSchema: json.RawMessage(`{
			"type": "object",
			"properties": {
				"context": {"type": "string"},
				"namespace": {"type": "string"},
				"pod": {"type": "string"},
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
			"required": ["context", "namespace", "pod", "container", "previous", "timestamps", "truncated", "bytes_returned", "bytes_streamed", "logs"]
		}`),
		Annotations: readOnly(),
	}
}

// NewGetPodLogsHandler returns the kubectl_get_pod_logs handler.
func NewGetPodLogsHandler(c kube.Clients) mcp.ToolFunction {
	return func(ctx context.Context, req *mcp.ToolRequest) (mcp.Result, error) {
		var args struct {
			Context    string `json:"context"`
			Namespace  string `json:"namespace"`
			Pod        string `json:"pod"`
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
		if args.Namespace == "" || args.Pod == "" {
			return mcp.ErrorResultf("the namespace and pod arguments are both required: use kubectl_get_pods to list pods"), nil
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

		container, errResult := resolveContainer(ctx, cs, args.Namespace, args.Pod, args.Container)
		if errResult != nil {
			return errResult, nil
		}

		fetched, errResult := fetchPodLogs(ctx, cs, args.Context, args.Namespace, args.Pod, container, window)
		if errResult != nil {
			return errResult, nil
		}

		out := podLogsOutput{
			Context:       args.Context,
			Namespace:     args.Namespace,
			Pod:           args.Pod,
			Container:     container,
			Previous:      window.Previous,
			Timestamps:    window.Timestamps,
			TailLines:     window.TailLines,
			MaxSizeKiB:    window.MaxSizeKiB,
			Truncated:     fetched.Truncated,
			BytesReturned: fetched.BytesReturned,
			BytesStreamed: fetched.BytesStreamed,
			Logs:          string(fetched.Logs),
		}

		return &mcp.ToolCallResult{
			Content: []mcp.Content{
				mcp.Text(logText(fetched, args.Namespace, args.Pod, container)),
				mcp.ResourceLinkContent(podURI(args.Context, args.Namespace, args.Pod),
					args.Pod, "Full Pod manifest", "application/json"),
			},
			StructuredContent: out,
		}, nil
	}
}

// logWindow is the resolved read-and-tail window shared by every tool that
// returns container logs: kubectl_get_pod_logs, which is handed a pod, and the
// plugin tools that find their own pod first.
type logWindow struct {
	TailLines  *int64
	MaxSizeKiB *int
	Previous   bool
	Timestamps bool
}

// resolveLogWindow validates the four window arguments and applies their
// defaults. Timestamps is a pointer because an omitted field must be
// distinguishable from an explicit false, the default being true.
func resolveLogWindow(tailLines *int64, maxSizeKiB *int, previous bool, timestamps *bool) (logWindow, *mcp.ToolCallResult) {
	if tailLines != nil && *tailLines < 1 {
		return logWindow{}, mcp.ErrorResultf("tail_lines must be at least 1, got %d: omit it for the whole retained log", *tailLines)
	}
	if maxSizeKiB != nil && (*maxSizeKiB < 1 || *maxSizeKiB > maxMaxSizeKiB) {
		return logWindow{}, mcp.ErrorResultf("max_size_kib must be between 1 and %d, got %d", maxMaxSizeKiB, *maxSizeKiB)
	}

	// Neither bound given: fall back to the 256 KiB tail rather than handing
	// back a whole log of unknown size.
	if maxSizeKiB == nil && tailLines == nil {
		d := defaultMaxSizeKiB
		maxSizeKiB = &d
	}

	w := logWindow{TailLines: tailLines, MaxSizeKiB: maxSizeKiB, Previous: previous, Timestamps: true}
	if timestamps != nil {
		w.Timestamps = *timestamps
	}
	return w, nil
}

// logFetch is what streaming one container's log yielded. It deliberately does
// not carry a JSON shape: each tool maps it into its own output struct, so the
// two OutputSchemas stay independent of each other.
type logFetch struct {
	Truncated     bool
	BytesReturned int
	BytesStreamed int64
	Logs          []byte
}

// fetchPodLogs streams one container's log subresource through a tailBuffer,
// applying window. kubeContext is carried only to name the cluster in the
// errors a model reads.
func fetchPodLogs(ctx context.Context, cs kubernetes.Interface, kubeContext, namespace, pod, container string, w logWindow) (logFetch, *mcp.ToolCallResult) {
	opts := &corev1.PodLogOptions{
		Container:  container,
		Previous:   w.Previous,
		Timestamps: w.Timestamps,
		TailLines:  w.TailLines,
		// LimitBytes is deliberately never set: it truncates from the
		// START of the stream, which is the opposite of a tail.
	}
	stream, err := cs.CoreV1().Pods(namespace).GetLogs(pod, opts).Stream(ctx)
	if err != nil {
		if w.Previous {
			return logFetch{}, mcp.ErrorResultf("failed to read previous logs for container %q of pod %s/%s on context %q (a pod that has never restarted has no previous container): %v",
				container, namespace, pod, kubeContext, err)
		}
		return logFetch{}, mcp.ErrorResultf("failed to read logs for container %q of pod %s/%s on context %q: %v",
			container, namespace, pod, kubeContext, err)
	}
	defer stream.Close()

	tb := newTailBuffer(w.MaxSizeKiB)
	if _, err := io.Copy(tb, stream); err != nil {
		return logFetch{}, mcp.ErrorResultf("failed while streaming logs for container %q of pod %s/%s: %v",
			container, namespace, pod, err)
	}
	logs, truncated := tb.Tail()
	return logFetch{
		Truncated:     truncated,
		BytesReturned: len(logs),
		BytesStreamed: tb.Total(),
		Logs:          logs,
	}, nil
}

// logText renders the text Content block a log tool returns: any caller-supplied
// banners, then the truncation banner, then the empty-log note, then the raw
// bytes.
//
// Built by hand rather than through jsonResult because that helper's text block
// is the JSON encoding, which would escape every newline in the log and leave
// it unreadable.
func logText(f logFetch, namespace, pod, container string, banners ...string) string {
	var text strings.Builder
	for _, b := range banners {
		text.WriteString(b)
		text.WriteString("\n")
	}
	if f.Truncated {
		fmt.Fprintf(&text, "[truncated: showing the last %d bytes of %d streamed]\n", f.BytesReturned, f.BytesStreamed)
	}
	if len(f.Logs) == 0 {
		fmt.Fprintf(&text, "[no log output from container %q of pod %s/%s]\n", container, namespace, pod)
	}
	text.Write(f.Logs)
	return text.String()
}

// resolveContainer validates the requested container against the pod, or picks
// the only one when the caller named none — the same courtesy `kubectl logs`
// extends. Fetching the pod first also turns a missing pod into a message
// naming it, instead of whatever the log subresource would have said.
func resolveContainer(ctx context.Context, cs kubernetes.Interface, namespace, podName, want string) (string, *mcp.ToolCallResult) {
	pod, err := cs.CoreV1().Pods(namespace).Get(ctx, podName, metav1.GetOptions{})
	if err != nil {
		if k8serrors.IsNotFound(err) {
			return "", mcp.ErrorResultf("pod %s/%s not found: use kubectl_get_pods to list pods", namespace, podName)
		}
		return "", mcp.ErrorResultf("failed to fetch pod %s/%s: %v", namespace, podName, err)
	}

	main := containerNames(pod.Spec.Containers)
	others := containerNames(pod.Spec.InitContainers)
	for _, ec := range pod.Spec.EphemeralContainers {
		others = append(others, ec.Name)
	}

	if want == "" {
		if len(main) == 1 {
			return main[0], nil
		}
		return "", mcp.ErrorResultf("pod %s/%s has %d containers, so the container argument is required: %s",
			namespace, podName, len(main), describeContainers(main, others))
	}
	for _, name := range append(append([]string{}, main...), others...) {
		if name == want {
			return want, nil
		}
	}
	return "", mcp.ErrorResultf("pod %s/%s has no container %q: %s",
		namespace, podName, want, describeContainers(main, others))
}

// describeContainers renders the candidate list carried by both container
// errors.
func describeContainers(main, others []string) string {
	msg := "containers are [" + strings.Join(main, " ") + "]"
	if len(others) > 0 {
		msg += ", plus init/ephemeral containers [" + strings.Join(others, " ") + "]"
	}
	return msg
}

// tailBuffer is an io.Writer that keeps only the trailing limit bytes of
// everything written to it, counting the whole stream as it goes. A limit of
// zero keeps everything.
//
// This exists because the Kubernetes log API cannot do it: PodLogOptions.
// LimitBytes truncates from the start of the stream, and TailLines counts
// lines, not bytes. The only way to bound the answer by bytes taken from the
// END is to read the stream and drop what falls out of the window.
type tailBuffer struct {
	limit int
	buf   []byte
	total int64
}

func newTailBuffer(limitKiB *int) *tailBuffer {
	limit := 0
	if limitKiB != nil {
		limit = *limitKiB * 1024
	}
	return &tailBuffer{limit: limit}
}

func (t *tailBuffer) Write(p []byte) (int, error) {
	t.total += int64(len(p))
	t.buf = append(t.buf, p...)
	// Compacted only past twice the limit, so the copy is amortized rather
	// than paid on every write; memory stays bounded at 2*limit.
	if t.limit > 0 && len(t.buf) > 2*t.limit {
		n := copy(t.buf, t.buf[len(t.buf)-t.limit:])
		t.buf = t.buf[:n]
	}
	return len(p), nil
}

// Total reports how many bytes were written, window or no window.
func (t *tailBuffer) Total() int64 { return t.total }

// Tail returns the retained bytes and whether anything was dropped. When the
// window clipped the front of the stream, the leading partial line goes too,
// so the result always starts at a line boundary.
func (t *tailBuffer) Tail() ([]byte, bool) {
	b := t.buf
	if t.limit > 0 && len(b) > t.limit {
		b = b[len(b)-t.limit:]
	}
	truncated := int64(len(b)) < t.total
	if truncated {
		if i := bytes.IndexByte(b, '\n'); i >= 0 {
			b = b[i+1:]
		}
	}
	return b, truncated
}
