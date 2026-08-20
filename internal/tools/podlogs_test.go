package tools

import (
	"fmt"
	"strings"
	"testing"

	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/client-go/kubernetes/fake"
	k8stesting "k8s.io/client-go/testing"
)

// installLogReactor serves the pods/log subresource from the fake clientset and
// captures the PodLogOptions each request carried. The fake's GetLogs runs a
// generic "get" action on the log subresource and streams back whatever
// *runtime.Unknown the reactor chain returns (client-go
// kubernetes/typed/core/v1/fake/fake_pod_expansion.go), so this drives the real
// code path with no cluster. Returning handled=false for every other "get"
// leaves ordinary Pod fetches to the tracker.
func installLogReactor(t *testing.T, c *fakeClients, body string, streamErr error) *corev1.PodLogOptions {
	t.Helper()
	cs, ok := c.sets["test"].(*fake.Clientset)
	if !ok {
		t.Fatalf("expected a *fake.Clientset, got %T", c.sets["test"])
	}
	captured := &corev1.PodLogOptions{}
	cs.PrependReactor("get", "pods", func(action k8stesting.Action) (bool, runtime.Object, error) {
		if action.GetSubresource() != "log" {
			return false, nil, nil
		}
		if opts, ok := action.(k8stesting.GenericAction).GetValue().(*corev1.PodLogOptions); ok {
			*captured = *opts
		}
		if streamErr != nil {
			return true, nil, streamErr
		}
		return true, &runtime.Unknown{Raw: []byte(body)}, nil
	})
	return captured
}

// numberedLines builds a log body of n lines, each ~40 bytes, so a byte window
// lands mid-line and the line-boundary trim has something to do.
func numberedLines(n int) string {
	var b strings.Builder
	for i := 0; i < n; i++ {
		fmt.Fprintf(&b, "2026-08-19T14:02:%02dZ line %06d %s\n", i%60, i, strings.Repeat("x", 10))
	}
	return b.String()
}

func TestGetPodLogsSingleContainerDefaulting(t *testing.T) {
	c := newFakeClients(podWithContainers("a", "solo", []string{"app"}, nil))
	opts := installLogReactor(t, c, "hello\nworld\n", nil)

	res := callTool(t, NewGetPodLogsHandler(c), `{"context":"test","namespace":"a","pod":"solo"}`)
	if res.IsError {
		t.Fatalf("unexpected tool error: %s", contentText(t, res))
	}
	if opts.Container != "app" {
		t.Errorf("expected the sole container to be chosen, got %q", opts.Container)
	}
	out := res.StructuredContent.(podLogsOutput)
	if out.Logs != "hello\nworld\n" {
		t.Errorf("unexpected logs: %q", out.Logs)
	}
	if out.Truncated {
		t.Error("a log that fits must not report truncation")
	}
	if out.BytesReturned != 12 || out.BytesStreamed != 12 {
		t.Errorf("unexpected byte counts: returned=%d streamed=%d", out.BytesReturned, out.BytesStreamed)
	}
	// Text block is the raw log, plus a link to the pod manifest.
	if got := contentText(t, res); got != "hello\nworld\n" {
		t.Errorf("unexpected text content: %q", got)
	}
	if len(res.Content) != 2 || res.Content[1].Type != "resource_link" {
		t.Errorf("expected a text block and a resource_link, got %+v", res.Content)
	}
}

// An empty log is a legitimate answer, but an empty text block reads like a
// broken tool, so it is said out loud.
func TestGetPodLogsEmptyOutputSaysSo(t *testing.T) {
	c := newFakeClients(podWithContainers("a", "quiet", []string{"app"}, nil))
	installLogReactor(t, c, "", nil)

	res := callTool(t, NewGetPodLogsHandler(c), `{"context":"test","namespace":"a","pod":"quiet"}`)
	if res.IsError {
		t.Fatalf("an empty log is not an error: %s", contentText(t, res))
	}
	if !strings.Contains(contentText(t, res), "no log output") {
		t.Errorf("expected the empty result to be explained: %q", contentText(t, res))
	}
	if out := res.StructuredContent.(podLogsOutput); out.Logs != "" || out.BytesReturned != 0 {
		t.Errorf("unexpected structured output: %+v", out)
	}
}

func TestGetPodLogsDefaultsToTimestampsAndTheDefaultWindow(t *testing.T) {
	c := newFakeClients(podWithContainers("a", "solo", []string{"app"}, nil))
	opts := installLogReactor(t, c, "line\n", nil)

	res := callTool(t, NewGetPodLogsHandler(c), `{"context":"test","namespace":"a","pod":"solo"}`)
	if !opts.Timestamps {
		t.Error("timestamps must default to on")
	}
	if opts.TailLines != nil {
		t.Errorf("no tail_lines argument must mean no TailLines, got %d", *opts.TailLines)
	}
	if opts.LimitBytes != nil {
		t.Error("LimitBytes truncates from the start of the stream and must never be set")
	}
	out := res.StructuredContent.(podLogsOutput)
	if out.MaxSizeKiB == nil || *out.MaxSizeKiB != defaultMaxSizeKiB {
		t.Errorf("expected the %d KiB default window to be reported, got %v", defaultMaxSizeKiB, out.MaxSizeKiB)
	}
}

func TestGetPodLogsTimestampsCanBeTurnedOff(t *testing.T) {
	c := newFakeClients(podWithContainers("a", "solo", []string{"app"}, nil))
	opts := installLogReactor(t, c, "line\n", nil)

	callTool(t, NewGetPodLogsHandler(c), `{"context":"test","namespace":"a","pod":"solo","timestamps":false}`)
	if opts.Timestamps {
		t.Error("an explicit timestamps:false must be distinguishable from an omitted field")
	}
}

// The four tail_lines / max_size_kib combinations, which are the whole point of
// the argument pair.
func TestGetPodLogsWindowCombinations(t *testing.T) {
	body := numberedLines(500) // ~20 KB

	cases := []struct {
		name          string
		args          string
		wantTailLines int64 // 0 = must be unset
		wantTruncated bool
		wantMaxBytes  int // 0 = unbounded
	}{
		{"neither", `{"context":"test","namespace":"a","pod":"solo"}`, 0, false, defaultMaxSizeKiB * 1024},
		{"tail_lines only", `{"context":"test","namespace":"a","pod":"solo","tail_lines":20}`, 20, false, 0},
		{"max_size_kib only", `{"context":"test","namespace":"a","pod":"solo","max_size_kib":4}`, 0, true, 4 * 1024},
		{"both", `{"context":"test","namespace":"a","pod":"solo","tail_lines":20,"max_size_kib":4}`, 20, true, 4 * 1024},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			c := newFakeClients(podWithContainers("a", "solo", []string{"app"}, nil))
			opts := installLogReactor(t, c, body, nil)

			res := callTool(t, NewGetPodLogsHandler(c), tc.args)
			if res.IsError {
				t.Fatalf("unexpected tool error: %s", contentText(t, res))
			}
			if tc.wantTailLines == 0 {
				if opts.TailLines != nil {
					t.Errorf("expected no TailLines, got %d", *opts.TailLines)
				}
			} else if opts.TailLines == nil || *opts.TailLines != tc.wantTailLines {
				t.Errorf("expected TailLines=%d, got %v", tc.wantTailLines, opts.TailLines)
			}
			if opts.LimitBytes != nil {
				t.Error("LimitBytes must never be set")
			}

			out := res.StructuredContent.(podLogsOutput)
			if out.Truncated != tc.wantTruncated {
				t.Errorf("truncated=%v, want %v (returned %d of %d bytes)",
					out.Truncated, tc.wantTruncated, out.BytesReturned, out.BytesStreamed)
			}
			if tc.wantMaxBytes > 0 && out.BytesReturned > tc.wantMaxBytes {
				t.Errorf("returned %d bytes, more than the %d-byte window", out.BytesReturned, tc.wantMaxBytes)
			}
			if !strings.HasSuffix(out.Logs, "line 000499 xxxxxxxxxx\n") {
				t.Error("the window must keep the END of the log")
			}
			if out.Truncated {
				if !strings.HasPrefix(out.Logs, "2026-08-19T") {
					t.Errorf("a truncated window must start at a line boundary, got %q", out.Logs[:24])
				}
				if !strings.Contains(contentText(t, res), "[truncated:") {
					t.Error("a truncated result must say so in its text block")
				}
			}
		})
	}
}

func TestGetPodLogsPreviousReachesTheAPI(t *testing.T) {
	c := newFakeClients(podWithContainers("a", "solo", []string{"app"}, nil))
	opts := installLogReactor(t, c, "old\n", nil)

	res := callTool(t, NewGetPodLogsHandler(c), `{"context":"test","namespace":"a","pod":"solo","previous":true}`)
	if res.IsError {
		t.Fatalf("unexpected tool error: %s", contentText(t, res))
	}
	if !opts.Previous {
		t.Error("previous:true must reach PodLogOptions")
	}
	if !res.StructuredContent.(podLogsOutput).Previous {
		t.Error("the result must echo which container generation was read")
	}
}

// A pod that never restarted has no previous container; the API says so and the
// model has to be able to act on it, so it must be a tool error.
func TestGetPodLogsPreviousUnavailableIsToolError(t *testing.T) {
	c := newFakeClients(podWithContainers("a", "solo", []string{"app"}, nil))
	installLogReactor(t, c, "", fmt.Errorf("previous terminated container \"app\" in pod \"solo\" not found"))

	res := callTool(t, NewGetPodLogsHandler(c), `{"context":"test","namespace":"a","pod":"solo","previous":true}`)
	if !res.IsError {
		t.Fatal("expected a tool error")
	}
	if !strings.Contains(contentText(t, res), "never restarted") {
		t.Errorf("the error should explain what previous means: %s", contentText(t, res))
	}
}

func TestGetPodLogsMultiContainerRequiresAChoice(t *testing.T) {
	c := newFakeClients(podWithContainers("a", "multi", []string{"app", "exporter"}, []string{"wait-for-db"}))
	installLogReactor(t, c, "unused\n", nil)

	res := callTool(t, NewGetPodLogsHandler(c), `{"context":"test","namespace":"a","pod":"multi"}`)
	if !res.IsError {
		t.Fatal("expected a tool error when the container is ambiguous")
	}
	msg := contentText(t, res)
	for _, want := range []string{"app", "exporter", "wait-for-db"} {
		if !strings.Contains(msg, want) {
			t.Errorf("the error must name the candidate %q: %s", want, msg)
		}
	}
}

func TestGetPodLogsInitContainerIsSelectable(t *testing.T) {
	c := newFakeClients(podWithContainers("a", "multi", []string{"app"}, []string{"wait-for-db"}))
	opts := installLogReactor(t, c, "waiting\n", nil)

	res := callTool(t, NewGetPodLogsHandler(c), `{"context":"test","namespace":"a","pod":"multi","container":"wait-for-db"}`)
	if res.IsError {
		t.Fatalf("unexpected tool error: %s", contentText(t, res))
	}
	if opts.Container != "wait-for-db" {
		t.Errorf("expected the init container to be requested, got %q", opts.Container)
	}
}

func TestGetPodLogsUnknownContainerIsToolError(t *testing.T) {
	c := newFakeClients(podWithContainers("a", "solo", []string{"app"}, nil))
	installLogReactor(t, c, "unused\n", nil)

	res := callTool(t, NewGetPodLogsHandler(c), `{"context":"test","namespace":"a","pod":"solo","container":"nope"}`)
	if !res.IsError {
		t.Fatal("expected a tool error for an unknown container")
	}
	if !strings.Contains(contentText(t, res), "app") {
		t.Errorf("the error must name the real containers: %s", contentText(t, res))
	}
}

func TestGetPodLogsMissingPodIsToolError(t *testing.T) {
	c := newFakeClients()
	res := callTool(t, NewGetPodLogsHandler(c), `{"context":"test","namespace":"a","pod":"ghost"}`)
	if !res.IsError {
		t.Fatal("expected a tool error for a missing pod")
	}
	if !strings.Contains(contentText(t, res), "kubectl_get_pods") {
		t.Errorf("the error should point at the tool that lists pods: %s", contentText(t, res))
	}
}

func TestGetPodLogsArgumentValidation(t *testing.T) {
	c := newFakeClients(podWithContainers("a", "solo", []string{"app"}, nil))
	for _, args := range []string{
		`{"context":"nope","namespace":"a","pod":"solo"}`,
		`{"context":"test","pod":"solo"}`,
		`{"context":"test","namespace":"a"}`,
		`{"context":"test","namespace":"a","pod":"solo","tail_lines":0}`,
		`{"context":"test","namespace":"a","pod":"solo","max_size_kib":0}`,
		fmt.Sprintf(`{"context":"test","namespace":"a","pod":"solo","max_size_kib":%d}`, maxMaxSizeKiB+1),
	} {
		if res := callTool(t, NewGetPodLogsHandler(c), args); !res.IsError {
			t.Errorf("expected a tool error for %s", args)
		}
	}
}

func TestTailBuffer(t *testing.T) {
	t.Run("keeps the last limit bytes at a line boundary", func(t *testing.T) {
		tb := newTailBuffer(ptrTo(1)) // 1 KiB
		body := numberedLines(200)    // ~8 KB
		// Written in small chunks, the way io.Copy feeds it.
		for i := 0; i < len(body); i += 97 {
			end := i + 97
			if end > len(body) {
				end = len(body)
			}
			if _, err := tb.Write([]byte(body[i:end])); err != nil {
				t.Fatal(err)
			}
		}
		got, truncated := tb.Tail()
		if !truncated {
			t.Fatal("expected truncation")
		}
		if len(got) > 1024 {
			t.Errorf("kept %d bytes, more than the 1024-byte window", len(got))
		}
		if !strings.HasSuffix(string(got), body[len(body)-64:]) {
			t.Error("the kept bytes must be the tail of the stream")
		}
		if !strings.HasPrefix(string(got), "2026-08-19T") {
			t.Errorf("expected a line boundary, got %q", string(got)[:24])
		}
		// The dropped partial line means at most one line's worth below the cap.
		if len(got) < 1024-40 {
			t.Errorf("kept only %d bytes, expected close to the full window", len(got))
		}
		if tb.Total() != int64(len(body)) {
			t.Errorf("Total()=%d, want %d", tb.Total(), len(body))
		}
	})

	t.Run("no truncation when the stream fits", func(t *testing.T) {
		tb := newTailBuffer(ptrTo(1))
		tb.Write([]byte("short\n"))
		got, truncated := tb.Tail()
		if truncated || string(got) != "short\n" {
			t.Errorf("got %q truncated=%v", got, truncated)
		}
	})

	t.Run("a nil limit keeps everything", func(t *testing.T) {
		tb := newTailBuffer(nil)
		body := numberedLines(1000)
		tb.Write([]byte(body))
		got, truncated := tb.Tail()
		if truncated || string(got) != body {
			t.Errorf("an unlimited buffer must keep the whole stream (kept %d of %d)", len(got), len(body))
		}
	})

	t.Run("one line longer than the window is kept as-is", func(t *testing.T) {
		tb := newTailBuffer(ptrTo(1))
		tb.Write([]byte(strings.Repeat("y", 4096)))
		got, truncated := tb.Tail()
		if !truncated {
			t.Fatal("expected truncation")
		}
		if len(got) != 1024 {
			t.Errorf("with no newline to trim to, the whole window is kept: got %d bytes", len(got))
		}
	})
}

func ptrTo[T any](v T) *T { return &v }
