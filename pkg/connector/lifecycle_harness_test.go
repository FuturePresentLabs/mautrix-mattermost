package connector

import (
	"sync"
	"testing"

	"github.com/rs/zerolog"
	"maunium.net/go/mautrix/bridgev2"
	"maunium.net/go/mautrix/bridgev2/bridgeconfig"
	"maunium.net/go/mautrix/bridgev2/database"

	"github.com/bostrot/mautrix-mattermost/pkg/mattermost"
)

// eventRecorder is the test stand-in for UserLogin.QueueRemoteEvent.
type eventRecorder struct {
	mu     sync.Mutex
	events []bridgev2.RemoteEvent
	// result overrides the framework's answer (the default is Queued, which is
	// what bridgev2 returns when the event reached the portal lane).
	result *bridgev2.EventHandlingResult
}

func (r *eventRecorder) queue(ev bridgev2.RemoteEvent) bridgev2.EventHandlingResult {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.events = append(r.events, ev)
	if r.result != nil {
		return *r.result
	}
	return bridgev2.EventHandlingResultQueued
}

func (r *eventRecorder) len() int {
	r.mu.Lock()
	defer r.mu.Unlock()
	return len(r.events)
}

// drain returns the queued events in FIFO order and empties the queue.
func (r *eventRecorder) drain() []bridgev2.RemoteEvent {
	r.mu.Lock()
	defer r.mu.Unlock()
	out := r.events
	r.events = nil
	return out
}

// chatDeletes reports RemoteChatDelete events on the queue.
func (r *eventRecorder) chatDeletes() []*mmChatDeleteEvent {
	r.mu.Lock()
	defer r.mu.Unlock()
	var out []*mmChatDeleteEvent
	for _, ev := range r.events {
		if del, ok := ev.(*mmChatDeleteEvent); ok {
			out = append(out, del)
		}
	}
	return out
}

type lifecycleOptions struct {
	// noLogin leaves UserLogin nil (no bridge at all).
	noLogin bool
	// noBridgeConfig attaches a login whose bridge has no config.
	noBridgeConfig bool
	// asyncEvents sets Bridge.Config.AsyncEvents.
	asyncEvents bool
	// inlineEvents sets Bridge.Config.PortalEventBuffer to 0.
	inlineEvents bool
}

// lifecycleHarness wires a MattermostClient to a recording event queue and a
// bridge configuration, so tests can drive the real event loop.
type lifecycleHarness struct {
	client *MattermostClient
	events *eventRecorder
}

func newLifecycleHarness(t *testing.T, opts lifecycleOptions) *lifecycleHarness {
	t.Helper()
	h := &lifecycleHarness{events: &eventRecorder{}}
	c := &MattermostClient{
		Token:     "tok",
		UserID:    "test-user",
		ServerURL: "http://unused.invalid",
		baseURL:   "http://unused.invalid",
	}
	c.queueRemoteEventFn = h.events.queue

	if !opts.noLogin {
		br := &bridgev2.Bridge{Log: zerolog.Nop()}
		if !opts.noBridgeConfig {
			buffer := 64 // the shipped default (portal_event_buffer)
			if opts.inlineEvents {
				buffer = 0
			}
			br.Config = &bridgeconfig.BridgeConfig{AsyncEvents: opts.asyncEvents, PortalEventBuffer: buffer}
		}
		c.UserLogin = &bridgev2.UserLogin{
			UserLogin: &database.UserLogin{ID: "login-test"},
			Bridge:    br,
			Log:       zerolog.Nop(),
		}
	}
	h.client = c
	return h
}

// queuedTypes lists the types of everything the connector queued, in order.
func (h *lifecycleHarness) queuedTypes() []bridgev2.RemoteEventType {
	var out []bridgev2.RemoteEventType
	for _, ev := range h.events.drain() {
		out = append(out, ev.GetType())
	}
	return out
}

// processEventsHarness runs the connector's real processEvents loop over a fake
// Mattermost WebSocket feed.
type processEventsHarness struct {
	*lifecycleHarness
	feed chan any
	done chan struct{}
}

func newProcessEventsHarness(t *testing.T, opts lifecycleOptions) *processEventsHarness {
	t.Helper()
	h := newLifecycleHarness(t, opts)
	feed := make(chan any, 64)
	h.client.mu.Lock()
	h.client.client = &mattermost.Client{Events: feed}
	h.client.mu.Unlock()

	done := make(chan struct{})
	go func() {
		defer close(done)
		h.client.processEvents()
	}()
	t.Cleanup(func() {
		select {
		case <-done:
		default:
			close(feed)
			<-done
		}
	})
	return &processEventsHarness{lifecycleHarness: h, feed: feed, done: done}
}

// deliver feeds events through the real loop in order and returns once
// processEvents has handled all of them (it returns when the feed closes).
func (p *processEventsHarness) deliver(events ...any) {
	for _, ev := range events {
		p.feed <- ev
	}
	close(p.feed)
	<-p.done
}
