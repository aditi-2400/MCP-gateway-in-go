package progress

import (
	"encoding/json"
	"sync"
	"testing"

	"github.com/aditi-2400/MCP-gateway-in-go/internal/jsonrpc"
)

// collector records what a tracked request was sent.
type collector struct {
	mu   sync.Mutex
	msgs []*jsonrpc.Message
}

func (c *collector) send(msg *jsonrpc.Message) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.msgs = append(c.msgs, msg)
}

func (c *collector) params() []string {
	c.mu.Lock()
	defer c.mu.Unlock()
	var out []string
	for _, m := range c.msgs {
		out = append(out, string(m.Params))
	}
	return out
}

func tokenOf(t *testing.T, params json.RawMessage) json.RawMessage {
	t.Helper()
	var p struct {
		Meta map[string]json.RawMessage `json:"_meta"`
	}
	if err := json.Unmarshal(params, &p); err != nil {
		t.Fatalf("decode params %s: %v", params, err)
	}
	return p.Meta["progressToken"]
}

func progressFrom(token json.RawMessage, n int) *jsonrpc.Message {
	return jsonrpc.NewNotification("notifications/progress", json.RawMessage(
		`{"progressToken":`+string(token)+`,"progress":`+string(rune('0'+n))+`,"message":"step"}`))
}

func TestTrackWithoutTokenChangesNothing(t *testing.T) {
	tr := NewTracker()
	for _, params := range []string{`{"name":"x","_meta":{"a":1}}`, `{"name":"x"}`, ``, `{"_meta":null}`} {
		got, release, tracked := tr.Track(json.RawMessage(params), (&collector{}).send)
		release()
		if tracked || string(got) != params {
			t.Errorf("Track(%s) = %s, tracked %v, want unchanged and untracked", params, got, tracked)
		}
	}
}

func TestTrackReplacesOnlyTheToken(t *testing.T) {
	tr := NewTracker()
	params := json.RawMessage(`{"name":"fs__x","arguments":{"a":[1,2]},"_meta":{"progressToken":"client-1","io.modelcontextprotocol/protocolVersion":"2026-07-28","other":{"k":true}}}`)
	got, release, tracked := tr.Track(params, (&collector{}).send)
	defer release()
	if !tracked {
		t.Fatal("tracked = false, want true")
	}

	var before, after map[string]json.RawMessage
	if err := json.Unmarshal(params, &before); err != nil {
		t.Fatal(err)
	}
	if err := json.Unmarshal(got, &after); err != nil {
		t.Fatal(err)
	}
	for _, key := range []string{"name", "arguments"} {
		if string(after[key]) != string(before[key]) {
			t.Errorf("%s = %s, want %s", key, after[key], before[key])
		}
	}
	var beforeMeta, afterMeta map[string]json.RawMessage
	json.Unmarshal(before["_meta"], &beforeMeta)
	json.Unmarshal(after["_meta"], &afterMeta)
	for key, value := range beforeMeta {
		if key == "progressToken" {
			continue
		}
		if string(afterMeta[key]) != string(value) {
			t.Errorf("_meta.%s = %s, want %s", key, afterMeta[key], value)
		}
	}
	if string(afterMeta["progressToken"]) == `"client-1"` {
		t.Error("progressToken was not replaced")
	}
}

func TestDeliverRestoresTheClientToken(t *testing.T) {
	for _, original := range []string{`"client-1"`, `42`, `""`} {
		t.Run(original, func(t *testing.T) {
			tr := NewTracker()
			c := &collector{}
			params, release, _ := tr.Track(json.RawMessage(`{"_meta":{"progressToken":`+original+`}}`), c.send)
			defer release()

			if !tr.Deliver(progressFrom(tokenOf(t, params), 1)) {
				t.Fatal("Deliver() = false, want true for a progress notification")
			}
			got := c.params()
			want := `{"message":"step","progress":1,"progressToken":` + original + `}`
			if len(got) != 1 || got[0] != want {
				t.Fatalf("client got %v, want [%s]", got, want)
			}
		})
	}
}

func TestSameClientTokenFromTwoRequestsStaysSeparate(t *testing.T) {
	// Two HTTP clients can both pick progress token "1".
	tr := NewTracker()
	a, b := &collector{}, &collector{}
	pa, releaseA, _ := tr.Track(json.RawMessage(`{"_meta":{"progressToken":"1"}}`), a.send)
	defer releaseA()
	pb, releaseB, _ := tr.Track(json.RawMessage(`{"_meta":{"progressToken":"1"}}`), b.send)
	defer releaseB()

	if string(tokenOf(t, pa)) == string(tokenOf(t, pb)) {
		t.Fatalf("both requests got upstream token %s", tokenOf(t, pa))
	}
	tr.Deliver(progressFrom(tokenOf(t, pb), 1))
	if len(a.params()) != 0 || len(b.params()) != 1 {
		t.Fatalf("a got %v, b got %v, want only b to get one", a.params(), b.params())
	}
}

func TestDeliverIgnoresOtherAndUnknownNotifications(t *testing.T) {
	tr := NewTracker()
	c := &collector{}
	params, release, _ := tr.Track(json.RawMessage(`{"_meta":{"progressToken":"t"}}`), c.send)

	if tr.Deliver(jsonrpc.NewNotification("notifications/message", json.RawMessage(`{"level":"info"}`))) {
		t.Error("Deliver(notifications/message) = true, want false so the caller handles it")
	}
	// Progress for tokens the gateway never handed out, or that have been
	// released, is dropped (true means "handled, do not forward").
	for _, n := range []*jsonrpc.Message{
		progressFrom(json.RawMessage(`"not-ours"`), 1),
		progressFrom(json.RawMessage(`7`), 1),
		jsonrpc.NewNotification("notifications/progress", json.RawMessage(`{"progress":1}`)),
		jsonrpc.NewNotification("notifications/progress", json.RawMessage(`[1]`)),
	} {
		if !tr.Deliver(n) {
			t.Errorf("Deliver(%s) = false, want true (dropped)", n.Params)
		}
	}
	release()
	if !tr.Deliver(progressFrom(tokenOf(t, params), 2)) {
		t.Error("Deliver after release = false, want true (dropped)")
	}
	if got := c.params(); len(got) != 0 {
		t.Fatalf("client got %v, want nothing", got)
	}
}

func TestStreamDropsMessagesAfterFinish(t *testing.T) {
	c := &collector{}
	s := NewStream(c.send)
	s.Send(jsonrpc.NewNotification("notifications/progress", json.RawMessage(`{"progress":1}`)))
	finalRan := false
	s.Finish(func() { finalRan = true })
	s.Send(jsonrpc.NewNotification("notifications/progress", json.RawMessage(`{"progress":2}`)))

	if !finalRan {
		t.Error("Finish did not run the final write")
	}
	if got := c.params(); len(got) != 1 || got[0] != `{"progress":1}` {
		t.Fatalf("sent %v, want only the progress before Finish", got)
	}
}

func TestTrackIsSafeForConcurrentUse(t *testing.T) {
	tr := NewTracker()
	var wg sync.WaitGroup
	seen := sync.Map{}
	for range 50 {
		wg.Go(func() {
			params, release, _ := tr.Track(json.RawMessage(`{"_meta":{"progressToken":"same"}}`), (&collector{}).send)
			defer release()
			var p struct {
				Meta struct {
					ProgressToken string `json:"progressToken"`
				} `json:"_meta"`
			}
			json.Unmarshal(params, &p)
			if _, dup := seen.LoadOrStore(p.Meta.ProgressToken, true); dup {
				t.Errorf("token %s handed out twice", p.Meta.ProgressToken)
			}
			tr.Deliver(progressFrom(json.RawMessage(`"`+p.Meta.ProgressToken+`"`), 1))
		})
	}
	wg.Wait()
}

func TestStripRemovesTheToken(t *testing.T) {
	got := Strip(json.RawMessage(`{"progressToken":"x","io.modelcontextprotocol/protocolVersion":"2026-07-28"}`))
	if string(got) != `{"io.modelcontextprotocol/protocolVersion":"2026-07-28"}` {
		t.Fatalf("Strip() = %s", got)
	}
	for _, in := range []string{`{"a":1}`, ``, `null`} {
		if got := Strip(json.RawMessage(in)); string(got) != in {
			t.Errorf("Strip(%s) = %s, want unchanged", in, got)
		}
	}
}
