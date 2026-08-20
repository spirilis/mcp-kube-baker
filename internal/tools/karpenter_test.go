package tools

import (
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/spirilis/generic-go-mcp/mcp"
	coordinationv1 "k8s.io/api/coordination/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
)

// The identity controller-runtime writes: the pod's hostname, which is its name,
// then an underscore and a uuid.
const testKarpenterHolder = "karpenter-6d9f7b5c4-abcde_11111111-2222-3333-4444-555555555555"

// karpenterLease builds a leader-election Lease. Every LeaseSpec field is a
// pointer, which is exactly why the handler nil-checks them.
func karpenterLease(namespace, name, holder string, renew time.Time, durationSec int32) *coordinationv1.Lease {
	l := &coordinationv1.Lease{ObjectMeta: metav1.ObjectMeta{Namespace: namespace, Name: name}}
	if holder != "" {
		l.Spec.HolderIdentity = ptrTo(holder)
	}
	if !renew.IsZero() {
		l.Spec.RenewTime = ptrTo(metav1.NewMicroTime(renew))
	}
	if durationSec != 0 {
		l.Spec.LeaseDurationSeconds = ptrTo(durationSec)
	}
	return l
}

// freshKarpenter is the ordinary fixture: a current Lease in kube-system and the
// single-container pod it names.
func freshKarpenter() []runtime.Object {
	return []runtime.Object{
		karpenterLease("kube-system", karpenterLeaseName, testKarpenterHolder, time.Now(), 15),
		podWithContainers("kube-system", "karpenter-6d9f7b5c4-abcde", []string{"controller"}, nil),
	}
}

func callKarpenter(t *testing.T, c *fakeClients, args string) (*mcp.ToolCallResult, karpenterLogsOutput) {
	t.Helper()
	res := callTool(t, NewKarpenterLogsHandler(c), args)
	if res.IsError {
		t.Fatalf("unexpected tool error: %s", contentText(t, res))
	}
	out, ok := res.StructuredContent.(karpenterLogsOutput)
	if !ok {
		t.Fatalf("expected karpenterLogsOutput, got %T", res.StructuredContent)
	}
	return res, out
}

func TestKarpenterLogsHappyPath(t *testing.T) {
	c := newFakeClients(freshKarpenter()...)
	opts := installLogReactor(t, c, "provisioning nodeclaim\n", nil)

	res, out := callKarpenter(t, c, `{"context":"test"}`)

	if out.Pod != "karpenter-6d9f7b5c4-abcde" {
		t.Errorf("pod = %q, want the pod named by holderIdentity", out.Pod)
	}
	if out.PodNamespace != "kube-system" || out.LeaseNamespace != "kube-system" {
		t.Errorf("namespaces = %q/%q, want kube-system", out.LeaseNamespace, out.PodNamespace)
	}
	if out.LeaseName != karpenterLeaseName {
		t.Errorf("lease_name = %q, want %q", out.LeaseName, karpenterLeaseName)
	}
	if out.HolderIdentity != testKarpenterHolder {
		t.Errorf("holder_identity = %q, want the raw Lease value", out.HolderIdentity)
	}
	if out.Container != "controller" {
		t.Errorf("container = %q, want controller", out.Container)
	}
	if out.Stale || out.StaleReason != "" {
		t.Errorf("a current Lease is not stale: %v %q", out.Stale, out.StaleReason)
	}
	if out.Logs != "provisioning nodeclaim\n" {
		t.Errorf("logs = %q", out.Logs)
	}
	// The log text must not be JSON-escaped, which is what the hand-built result
	// is for.
	if got := contentText(t, res); got != "provisioning nodeclaim\n" {
		t.Errorf("text content = %q", got)
	}
	if !opts.Timestamps {
		t.Error("timestamps should default to true")
	}
	if opts.LimitBytes != nil {
		t.Error("LimitBytes truncates from the start of the stream and must never be set")
	}
}

func TestKarpenterLogsWindowArguments(t *testing.T) {
	tests := []struct {
		name           string
		args           string
		wantTailLines  *int64
		wantMaxSizeKiB *int
		wantPrevious   bool
		wantTimestamps bool
	}{
		{
			// Neither bound given: the 256 KiB tail, rather than a log of
			// unknown size.
			name:           "defaults",
			args:           `{"context":"test"}`,
			wantMaxSizeKiB: ptrTo(defaultMaxSizeKiB),
			wantTimestamps: true,
		},
		{
			// tail_lines alone is how a caller asks for everything: no byte cap
			// is applied on top of it.
			name:           "tail_lines alone leaves bytes unbounded",
			args:           `{"context":"test","tail_lines":20}`,
			wantTailLines:  ptrTo(int64(20)),
			wantTimestamps: true,
		},
		{
			name:           "both bounds",
			args:           `{"context":"test","tail_lines":20,"max_size_kib":8}`,
			wantTailLines:  ptrTo(int64(20)),
			wantMaxSizeKiB: ptrTo(8),
			wantTimestamps: true,
		},
		{
			name:           "previous and timestamps off",
			args:           `{"context":"test","previous":true,"timestamps":false}`,
			wantMaxSizeKiB: ptrTo(defaultMaxSizeKiB),
			wantPrevious:   true,
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			c := newFakeClients(freshKarpenter()...)
			opts := installLogReactor(t, c, "line\n", nil)

			_, out := callKarpenter(t, c, tc.args)

			if !equalPtr(out.TailLines, tc.wantTailLines) {
				t.Errorf("tail_lines = %v, want %v", deref(out.TailLines), deref(tc.wantTailLines))
			}
			if !equalPtr(out.MaxSizeKiB, tc.wantMaxSizeKiB) {
				t.Errorf("max_size_kib = %v, want %v", deref(out.MaxSizeKiB), deref(tc.wantMaxSizeKiB))
			}
			if out.Previous != tc.wantPrevious || out.Timestamps != tc.wantTimestamps {
				t.Errorf("previous/timestamps = %v/%v, want %v/%v",
					out.Previous, out.Timestamps, tc.wantPrevious, tc.wantTimestamps)
			}
			// What actually reached the API is the part that matters.
			if !equalPtr(opts.TailLines, tc.wantTailLines) {
				t.Errorf("PodLogOptions.TailLines = %v, want %v", deref(opts.TailLines), deref(tc.wantTailLines))
			}
			if opts.Previous != tc.wantPrevious || opts.Timestamps != tc.wantTimestamps {
				t.Errorf("PodLogOptions previous/timestamps = %v/%v, want %v/%v",
					opts.Previous, opts.Timestamps, tc.wantPrevious, tc.wantTimestamps)
			}
			if opts.LimitBytes != nil {
				t.Error("LimitBytes must never be set")
			}
		})
	}
}

func TestKarpenterLogsArgumentValidation(t *testing.T) {
	tests := []struct{ name, args, want string }{
		{"tail_lines zero", `{"context":"test","tail_lines":0}`, "tail_lines must be at least 1"},
		{"max_size_kib zero", `{"context":"test","max_size_kib":0}`, "max_size_kib must be between"},
		{"max_size_kib too big", `{"context":"test","max_size_kib":8193}`, "max_size_kib must be between"},
		{"no context", `{}`, "context argument is required"},
		{"unknown context", `{"context":"nope"}`, "kubectl_get_contexts"},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			c := newFakeClients(freshKarpenter()...)
			res := callTool(t, NewKarpenterLogsHandler(c), tc.args)
			if !res.IsError {
				t.Fatal("expected a tool error")
			}
			if !strings.Contains(contentText(t, res), tc.want) {
				t.Errorf("error should mention %q: %s", tc.want, contentText(t, res))
			}
		})
	}
}

// TestKarpenterLogsFallsBackAcrossNamespaces covers an install whose
// leader-election Lease namespace differs from where its pods run.
//
// The fixture holds exactly one pod of that name on purpose: client-go's fake
// List applies only the label selector and silently drops the field selector, so
// a second same-named pod would be returned here and the test would be asserting
// against behaviour the fake does not emulate.
func TestKarpenterLogsFallsBackAcrossNamespaces(t *testing.T) {
	c := newFakeClients(
		karpenterLease("kube-system", karpenterLeaseName, testKarpenterHolder, time.Now(), 15),
		podWithContainers("karpenter", "karpenter-6d9f7b5c4-abcde", []string{"controller"}, nil),
	)
	installLogReactor(t, c, "found via fallback\n", nil)

	_, out := callKarpenter(t, c, `{"context":"test"}`)

	if out.PodNamespace != "karpenter" {
		t.Errorf("pod_namespace = %q, want the namespace the pod actually lives in", out.PodNamespace)
	}
	if out.LeaseNamespace != "kube-system" {
		t.Errorf("lease_namespace = %q, want kube-system", out.LeaseNamespace)
	}
}

func TestKarpenterLogsStaleLeaseIsReportedNotHidden(t *testing.T) {
	c := newFakeClients(
		karpenterLease("kube-system", karpenterLeaseName, testKarpenterHolder, time.Now().Add(-time.Hour), 15),
		podWithContainers("kube-system", "karpenter-6d9f7b5c4-abcde", []string{"controller"}, nil),
	)
	installLogReactor(t, c, "last words\n", nil)

	// An expired Lease is not an error: a Karpenter that stopped renewing
	// leadership is usually the thing being investigated.
	res, out := callKarpenter(t, c, `{"context":"test"}`)

	if !out.Stale {
		t.Error("an expired Lease should be reported stale")
	}
	if !strings.Contains(out.StaleReason, "EXPIRED") {
		t.Errorf("stale_reason should say what happened: %q", out.StaleReason)
	}
	text := contentText(t, res)
	if !strings.Contains(text, "EXPIRED") {
		t.Errorf("the text should lead with the staleness banner: %q", text)
	}
	if !strings.HasSuffix(text, "last words\n") {
		t.Errorf("the logs should still be returned: %q", text)
	}
}

func TestKarpenterLogsLeaseFreshnessUnknown(t *testing.T) {
	c := newFakeClients(
		// No renewTime and no duration: freshness cannot be established either
		// way, which must not be reported as "current".
		karpenterLease("kube-system", karpenterLeaseName, testKarpenterHolder, time.Time{}, 0),
		podWithContainers("kube-system", "karpenter-6d9f7b5c4-abcde", []string{"controller"}, nil),
	)
	installLogReactor(t, c, "line\n", nil)

	res, out := callKarpenter(t, c, `{"context":"test"}`)

	if out.Stale {
		t.Error("an undeterminable Lease is not known to be stale")
	}
	if !strings.Contains(out.StaleReason, "freshness unknown") {
		t.Errorf("stale_reason should say freshness is unknown: %q", out.StaleReason)
	}
	if !strings.Contains(contentText(t, res), "freshness unknown") {
		t.Error("the caveat belongs in the text too")
	}
	if out.RenewTime != "" {
		t.Errorf("renew_time should be omitted when the Lease has none, got %q", out.RenewTime)
	}
}

func TestKarpenterLogsFallbackFindsRenamedLease(t *testing.T) {
	c := newFakeClients(
		// A fork or renamed install: no karpenter-leader-election, but a Lease
		// whose name says what it is.
		karpenterLease("kube-system", "karpenter-core-leader-election", testKarpenterHolder, time.Now(), 15),
		podWithContainers("kube-system", "karpenter-6d9f7b5c4-abcde", []string{"controller"}, nil),
	)
	installLogReactor(t, c, "line\n", nil)

	_, out := callKarpenter(t, c, `{"context":"test"}`)

	// A fuzzy match must never be silent — the caller has to be able to see
	// which Lease this resolved through.
	if out.LeaseName != "karpenter-core-leader-election" {
		t.Errorf("lease_name = %q, want the Lease actually used", out.LeaseName)
	}
}

func TestKarpenterLogsLeaseErrors(t *testing.T) {
	tests := []struct {
		name    string
		objects []runtime.Object
		args    string
		want    []string
	}{
		{
			name:    "no lease at all",
			objects: []runtime.Object{podWithContainers("kube-system", "karpenter-6d9f7b5c4-abcde", []string{"controller"}, nil)},
			args:    `{"context":"test"}`,
			// Karpenter simply not being installed is the common case, and the
			// message has to say so rather than read like a failure.
			want: []string{karpenterLeaseName, "kube-system", "does not appear to be installed", "namespace argument"},
		},
		{
			name: "ambiguous fuzzy matches",
			objects: []runtime.Object{
				karpenterLease("kube-system", "karpenter-a-leader-election", testKarpenterHolder, time.Now(), 15),
				karpenterLease("kube-system", "karpenter-b-leader-election", testKarpenterHolder, time.Now(), 15),
			},
			args: `{"context":"test"}`,
			want: []string{"ambiguous", "karpenter-a-leader-election", "karpenter-b-leader-election"},
		},
		{
			name: "no holder",
			objects: []runtime.Object{
				karpenterLease("kube-system", karpenterLeaseName, "", time.Now(), 15),
			},
			args: `{"context":"test"}`,
			want: []string{"names no holder", "kubectl_get_pods"},
		},
		{
			name: "holder pod gone",
			objects: []runtime.Object{
				karpenterLease("kube-system", karpenterLeaseName, testKarpenterHolder, time.Now(), 15),
			},
			args: `{"context":"test"}`,
			want: []string{"karpenter-6d9f7b5c4-abcde", "kubectl_get_pods"},
		},
		{
			name: "wrong namespace argument",
			objects: []runtime.Object{
				karpenterLease("kube-system", karpenterLeaseName, testKarpenterHolder, time.Now(), 15),
			},
			args: `{"context":"test","namespace":"karpenter"}`,
			want: []string{`namespace "karpenter"`, "does not appear to be installed"},
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			c := newFakeClients(tc.objects...)
			installLogReactor(t, c, "line\n", nil)

			res := callTool(t, NewKarpenterLogsHandler(c), tc.args)
			if !res.IsError {
				t.Fatalf("expected a tool error, got %s", contentText(t, res))
			}
			for _, want := range tc.want {
				if !strings.Contains(contentText(t, res), want) {
					t.Errorf("error should mention %q: %s", want, contentText(t, res))
				}
			}
		})
	}
}

// TestKarpenterLogsHolderWithoutUUID covers a leader-election setup whose
// identity is a bare hostname: it is used whole rather than rejected.
func TestKarpenterLogsHolderWithoutUUID(t *testing.T) {
	c := newFakeClients(
		karpenterLease("kube-system", karpenterLeaseName, "karpenter-solo", time.Now(), 15),
		podWithContainers("kube-system", "karpenter-solo", []string{"controller"}, nil),
	)
	installLogReactor(t, c, "line\n", nil)

	_, out := callKarpenter(t, c, `{"context":"test"}`)
	if out.Pod != "karpenter-solo" {
		t.Errorf("pod = %q, want the whole holderIdentity used as the pod name", out.Pod)
	}
}

func TestKarpenterLogsContainerSelection(t *testing.T) {
	leader := func(containers ...string) []runtime.Object {
		return []runtime.Object{
			karpenterLease("kube-system", karpenterLeaseName, testKarpenterHolder, time.Now(), 15),
			podWithContainers("kube-system", "karpenter-6d9f7b5c4-abcde", containers, nil),
		}
	}

	t.Run("controller preferred over a sidecar", func(t *testing.T) {
		// The name preference is what keeps a no-argument call working if a
		// build ever grows a sidecar.
		c := newFakeClients(leader("metrics", "controller")...)
		installLogReactor(t, c, "line\n", nil)
		_, out := callKarpenter(t, c, `{"context":"test"}`)
		if out.Container != "controller" {
			t.Errorf("container = %q, want controller", out.Container)
		}
	})

	t.Run("sole container needs no argument", func(t *testing.T) {
		c := newFakeClients(leader("something-else")...)
		installLogReactor(t, c, "line\n", nil)
		_, out := callKarpenter(t, c, `{"context":"test"}`)
		if out.Container != "something-else" {
			t.Errorf("container = %q, want the only container", out.Container)
		}
	})

	t.Run("explicit container", func(t *testing.T) {
		c := newFakeClients(leader("metrics", "controller")...)
		installLogReactor(t, c, "line\n", nil)
		_, out := callKarpenter(t, c, `{"context":"test","container":"metrics"}`)
		if out.Container != "metrics" {
			t.Errorf("container = %q, want metrics", out.Container)
		}
	})

	t.Run("ambiguous without controller", func(t *testing.T) {
		c := newFakeClients(leader("a", "b")...)
		res := callTool(t, NewKarpenterLogsHandler(c), `{"context":"test"}`)
		if !res.IsError {
			t.Fatal("expected a tool error")
		}
		// Actionable because the container argument exists: never a dead end.
		for _, want := range []string{"container argument is required", "controller", "a", "b"} {
			if !strings.Contains(contentText(t, res), want) {
				t.Errorf("error should mention %q: %s", want, contentText(t, res))
			}
		}
	})

	t.Run("unknown container", func(t *testing.T) {
		c := newFakeClients(leader("controller")...)
		res := callTool(t, NewKarpenterLogsHandler(c), `{"context":"test","container":"nope"}`)
		if !res.IsError {
			t.Fatal("expected a tool error")
		}
		if !strings.Contains(contentText(t, res), "controller") {
			t.Errorf("error should name the real containers: %s", contentText(t, res))
		}
	})
}

func TestKarpenterLogsTruncationBannerFollowsStaleness(t *testing.T) {
	c := newFakeClients(
		karpenterLease("kube-system", karpenterLeaseName, testKarpenterHolder, time.Now().Add(-time.Hour), 15),
		podWithContainers("kube-system", "karpenter-6d9f7b5c4-abcde", []string{"controller"}, nil),
	)
	installLogReactor(t, c, numberedLines(500), nil)

	res, out := callKarpenter(t, c, `{"context":"test","max_size_kib":1}`)

	if !out.Truncated {
		t.Fatal("a 1 KiB window over 500 lines should truncate")
	}
	text := contentText(t, res)
	stale, truncated := strings.Index(text, "EXPIRED"), strings.Index(text, "[truncated:")
	if stale < 0 || truncated < 0 {
		t.Fatalf("both banners should be present: %q", text[:min(200, len(text))])
	}
	// Whether these logs are the current leader's changes how to read every line
	// below, so it comes first.
	if stale > truncated {
		t.Error("the staleness banner should precede the truncation banner")
	}
}

func TestKarpenterLogsEmptyLogIsExplained(t *testing.T) {
	c := newFakeClients(freshKarpenter()...)
	installLogReactor(t, c, "", nil)

	res, out := callKarpenter(t, c, `{"context":"test"}`)
	if out.Logs != "" {
		t.Errorf("logs = %q, want empty", out.Logs)
	}
	if !strings.Contains(contentText(t, res), "no log output") {
		t.Errorf("an empty log should be explained: %q", contentText(t, res))
	}
}

func TestKarpenterLogsStreamErrorIsToolError(t *testing.T) {
	c := newFakeClients(freshKarpenter()...)
	installLogReactor(t, c, "", fmt.Errorf("previous terminated container \"controller\" in pod \"karpenter-6d9f7b5c4-abcde\" not found"))

	res := callTool(t, NewKarpenterLogsHandler(c), `{"context":"test","previous":true}`)
	if !res.IsError {
		t.Fatal("expected a tool error")
	}
	if !strings.Contains(contentText(t, res), "never restarted") {
		t.Errorf("the error should explain what previous means: %s", contentText(t, res))
	}
}

func TestLeaseStaleness(t *testing.T) {
	now := time.Date(2026, 8, 20, 12, 0, 0, 0, time.UTC)
	tests := []struct {
		name      string
		lease     *coordinationv1.Lease
		wantStale bool
		wantMsg   string
	}{
		{
			name:  "current",
			lease: karpenterLease("kube-system", karpenterLeaseName, testKarpenterHolder, now.Add(-5*time.Second), 15),
		},
		{
			// Exactly at the boundary: renewTime+duration is not yet past.
			name:  "at expiry",
			lease: karpenterLease("kube-system", karpenterLeaseName, testKarpenterHolder, now.Add(-15*time.Second), 15),
		},
		{
			name:      "expired",
			lease:     karpenterLease("kube-system", karpenterLeaseName, testKarpenterHolder, now.Add(-16*time.Second), 15),
			wantStale: true,
			wantMsg:   "EXPIRED",
		},
		{
			name:    "no renewTime",
			lease:   karpenterLease("kube-system", karpenterLeaseName, testKarpenterHolder, time.Time{}, 15),
			wantMsg: "freshness unknown",
		},
		{
			name:    "no duration",
			lease:   karpenterLease("kube-system", karpenterLeaseName, testKarpenterHolder, now, 0),
			wantMsg: "freshness unknown",
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			stale, reason := leaseStaleness(tc.lease, now)
			if stale != tc.wantStale {
				t.Errorf("stale = %v, want %v", stale, tc.wantStale)
			}
			if tc.wantMsg == "" && reason != "" {
				t.Errorf("a current Lease needs no explanation, got %q", reason)
			}
			if tc.wantMsg != "" && !strings.Contains(reason, tc.wantMsg) {
				t.Errorf("reason = %q, want it to mention %q", reason, tc.wantMsg)
			}
		})
	}
}

func equalPtr[T comparable](a, b *T) bool {
	if a == nil || b == nil {
		return a == nil && b == nil
	}
	return *a == *b
}

func deref[T any](p *T) any {
	if p == nil {
		return nil
	}
	return *p
}
