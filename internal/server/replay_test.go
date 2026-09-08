package server

import (
	"encoding/json"
	"errors"
	"fmt"
	"testing"
	"time"

	"github.com/nats-io/nats.go"

	"nephtys/internal/connector"
	"nephtys/internal/domain"
	"nephtys/internal/store"
)

// The replay recipes in docs/REPLAY.md are a contract with consumers even
// though no Nephtys code implements them: they hold because events are
// published to a JetStream stream with the retention this service configures,
// on the subject the stream's config names. These tests register a stream
// through the real admission path, publish through the real pipeline and
// broker, and then replay exactly as the documented consumers do.

const replayTopic = "nephtys.stream.replay.test"

// replayEventCount is the number of events published before each replay. Six is
// enough to have a clear before/after either side of a start position without
// making the timestamps in the log unreadable.
const replayEventCount = 6

// publishSpacing separates published events so that no two of them share a
// broker store timestamp. Time-based replay is inclusive of its start instant,
// so events sharing a timestamp with the cut would both be delivered and the
// assertion below would be asserting nothing. A millisecond is above the
// coarsest clock granularity this test runs on.
const publishSpacing = 5 * time.Millisecond

// setupReplayStream registers a stream through Register, publishes
// replayEventCount events through its pipeline, and returns the manager's
// JetStream context.
func setupReplayStream(t *testing.T, id string) (*StreamManager, *nats.Conn) {
	t.Helper()

	srv := startTestNATS(t)
	brk := connectBroker(t, srv)
	if err := brk.EnsureStream("NEPHTYS", []string{"nephtys.stream.>"}); err != nil {
		t.Fatalf("ensure stream: %v", err)
	}

	st, err := store.NewStreamStore(brk.JetStream())
	if err != nil {
		t.Fatalf("new store: %v", err)
	}

	manager := NewStreamManager(brk, st)
	t.Cleanup(manager.StopAll)

	src := newMockSource(id)
	cfg := domain.StreamSourceConfig{ID: id, Kind: "websocket", Topic: replayTopic}
	if err := manager.Register(src, cfg); err != nil {
		t.Fatalf("register: %v", err)
	}

	var publish connector.PublishFunc
	select {
	case publish = <-src.publishes:
	case <-time.After(5 * time.Second):
		t.Fatal("source never started its session")
	}

	for i := 1; i <= replayEventCount; i++ {
		event := domain.StreamEvent{
			Source:    id,
			Type:      "reading",
			Timestamp: time.Now().UnixMilli(),
			Payload:   json.RawMessage(fmt.Sprintf(`{"n":%d}`, i)),
		}
		if err := publish(replayTopic, event); err != nil {
			t.Fatalf("publish %d: %v", i, err)
		}
		time.Sleep(publishSpacing)
	}

	// A separate client connection, so replaying looks to the broker exactly
	// like a consumer that is not this process.
	nc, err := nats.Connect(srv.ClientURL())
	if err != nil {
		t.Fatalf("consumer connect: %v", err)
	}
	t.Cleanup(nc.Close)
	return manager, nc
}

// replayed is one event as a replaying consumer sees it: where the broker put
// it, and what it carried.
type replayed struct {
	streamSeq uint64
	stored    time.Time
	n         int
}

// drain replays through sub until it stops producing, acknowledging as it goes
// — the same loop docs/replay/consumer.go runs.
func drain(t *testing.T, sub *nats.Subscription, limit int) []replayed {
	t.Helper()

	var out []replayed
	for len(out) < limit {
		msgs, err := sub.Fetch(limit-len(out), nats.MaxWait(2*time.Second))
		if err != nil {
			if errors.Is(err, nats.ErrTimeout) {
				break
			}
			t.Fatalf("fetch: %v", err)
		}
		for _, msg := range msgs {
			meta, err := msg.Metadata()
			if err != nil {
				t.Fatalf("metadata: %v", err)
			}
			var ev struct {
				Payload struct {
					N int `json:"n"`
				} `json:"payload"`
			}
			if err := json.Unmarshal(msg.Data, &ev); err != nil {
				t.Fatalf("decode event at seq %d: %v", meta.Sequence.Stream, err)
			}
			out = append(out, replayed{streamSeq: meta.Sequence.Stream, stored: meta.Timestamp, n: ev.Payload.N})
			// AckSync rather than Ack: a plain acknowledgement is published
			// without waiting, so a test that closes its connection straight
			// afterwards can race the acknowledgement floor it then asserts on.
			if err := msg.AckSync(); err != nil {
				t.Fatalf("ack: %v", err)
			}
		}
	}
	return out
}

// pullAll is the ephemeral pull consumer both documented tools create: bound to
// the stream by name, filtered by subject, positioned by one deliver policy.
func pullAll(t *testing.T, nc *nats.Conn, start nats.SubOpt) *nats.Subscription {
	t.Helper()

	js, err := nc.JetStream()
	if err != nil {
		t.Fatalf("jetstream: %v", err)
	}
	sub, err := js.PullSubscribe(replayTopic, "", nats.BindStream("NEPHTYS"), start)
	if err != nil {
		t.Fatalf("pull subscribe: %v", err)
	}
	t.Cleanup(func() { _ = sub.Drain() })
	return sub
}

// wantN asserts the replayed events carry exactly the payload counters want, in
// order. Naming the events rather than counting them is what makes the
// assertion discriminate: a replay that ignored its start position would return
// the right number of events from the wrong place.
func wantN(t *testing.T, got []replayed, want []int) {
	t.Helper()

	if len(got) != len(want) {
		t.Fatalf("replayed %d event(s), want %d: got %v", len(got), len(want), counters(got))
	}
	for i := range want {
		if got[i].n != want[i] {
			t.Fatalf("replayed %v, want %v", counters(got), want)
		}
	}
}

func counters(events []replayed) []int {
	out := make([]int, len(events))
	for i, e := range events {
		out[i] = e.n
	}
	return out
}

// TestReplayFromStart is the baseline the other cases are read against: every
// event a registered stream published is in JetStream and comes back in order.
func TestReplayFromStart(t *testing.T) {
	_, nc := setupReplayStream(t, "replay-all")

	got := drain(t, pullAll(t, nc, nats.DeliverAll()), replayEventCount)
	wantN(t, got, []int{1, 2, 3, 4, 5, 6})
}

// TestReplayFromSequence covers the documented "start at a stream sequence"
// recipe — the coordinate an operator reads off `nats stream info`.
func TestReplayFromSequence(t *testing.T) {
	_, nc := setupReplayStream(t, "replay-seq")

	all := drain(t, pullAll(t, nc, nats.DeliverAll()), replayEventCount)
	wantN(t, all, []int{1, 2, 3, 4, 5, 6})

	// Sequences are the broker's, not the envelope's, so the fourth event's
	// sequence has to be read back rather than assumed to be 4.
	fourth := all[3].streamSeq
	got := drain(t, pullAll(t, nc, nats.StartSequence(fourth)), replayEventCount)
	wantN(t, got, []int{4, 5, 6})
}

// TestReplayFromTime covers the "start at an instant" recipe, which is what a
// backfill after an outage actually asks for. The start instant is inclusive.
func TestReplayFromTime(t *testing.T) {
	_, nc := setupReplayStream(t, "replay-time")

	all := drain(t, pullAll(t, nc, nats.DeliverAll()), replayEventCount)
	wantN(t, all, []int{1, 2, 3, 4, 5, 6})

	cut := all[3].stored
	got := drain(t, pullAll(t, nc, nats.StartTime(cut)), replayEventCount)
	wantN(t, got, []int{4, 5, 6})
}

// TestReplayDurableResumesAfterOutage is the consumer-outage recovery contract:
// a durable consumer that stops part-way through resumes at its acknowledgement
// floor, with no redelivery and nothing to reconfigure.
//
// The outage is modelled by closing the consumer's connection, which is what a
// crashed process does. It is deliberately not modelled by draining or
// unsubscribing the subscription: the NATS client deletes the JetStream
// consumer behind any subscription it created when that subscription is drained
// — a durable one included — so a "clean" shutdown written that way destroys the
// very floor this test is about. docs/replay/consumer.go avoids it for the same
// reason.
func TestReplayDurableResumesAfterOutage(t *testing.T) {
	_, nc := setupReplayStream(t, "replay-durable")
	url := nc.ConnectedUrl()

	const durable = "warehouse-loader"

	// First run: consume two events, acknowledge them, then lose the process.
	before, err := nats.Connect(url)
	if err != nil {
		t.Fatalf("first consumer connect: %v", err)
	}
	beforeJS, err := before.JetStream()
	if err != nil {
		t.Fatalf("jetstream: %v", err)
	}
	sub, err := beforeJS.PullSubscribe(replayTopic, durable, nats.BindStream("NEPHTYS"), nats.DeliverAll())
	if err != nil {
		t.Fatalf("pull subscribe: %v", err)
	}
	wantN(t, drain(t, sub, 2), []int{1, 2})
	before.Close()

	// Second run: the same durable name, no deliver policy of its own, and it
	// picks up at three. Nothing sleeps here on purpose — a push consumer would
	// have streamed the rest ahead of the acknowledgements and delivered
	// nothing until AckWait expired, which is why the documented recipe and the
	// example consumer both pull.
	after, err := nats.Connect(url)
	if err != nil {
		t.Fatalf("second consumer connect: %v", err)
	}
	t.Cleanup(after.Close)
	afterJS, err := after.JetStream()
	if err != nil {
		t.Fatalf("jetstream: %v", err)
	}
	resumed, err := afterJS.PullSubscribe(replayTopic, durable, nats.BindStream("NEPHTYS"))
	if err != nil {
		t.Fatalf("resume subscribe: %v", err)
	}

	wantN(t, drain(t, resumed, replayEventCount), []int{3, 4, 5, 6})
}

// TestReplayIsScopedToItsSubject guards the filter every recipe relies on: a
// consumer replaying one stream's subject sees that stream's events only, even
// though every Nephtys stream shares one JetStream stream.
func TestReplayIsScopedToItsSubject(t *testing.T) {
	_, nc := setupReplayStream(t, "replay-filter")

	js, err := nc.JetStream()
	if err != nil {
		t.Fatalf("jetstream: %v", err)
	}
	sub, err := js.PullSubscribe("nephtys.stream.replay.other", "", nats.BindStream("NEPHTYS"), nats.DeliverAll())
	if err != nil {
		t.Fatalf("pull subscribe: %v", err)
	}
	t.Cleanup(func() { _ = sub.Drain() })

	if got := drain(t, sub, replayEventCount); len(got) != 0 {
		t.Fatalf("replaying a different subject returned %d event(s): %v", len(got), counters(got))
	}
}

// TestReplaySurvivesStreamRemoval states what DELETE /v1/streams/{id} does not
// do: removing a stream stops ingest and drops its stored config, and leaves
// everything it published replayable until retention expires it.
func TestReplaySurvivesStreamRemoval(t *testing.T) {
	srv := startTestNATS(t)
	brk := connectBroker(t, srv)
	if err := brk.EnsureStream("NEPHTYS", []string{"nephtys.stream.>"}); err != nil {
		t.Fatalf("ensure stream: %v", err)
	}

	st, err := store.NewStreamStore(brk.JetStream())
	if err != nil {
		t.Fatalf("new store: %v", err)
	}
	manager := NewStreamManager(brk, st)
	t.Cleanup(manager.StopAll)

	src := newMockSource("replay-removed")
	cfg := domain.StreamSourceConfig{ID: "replay-removed", Kind: "websocket", Topic: replayTopic}
	if err := manager.Register(src, cfg); err != nil {
		t.Fatalf("register: %v", err)
	}

	var publish connector.PublishFunc
	select {
	case publish = <-src.publishes:
	case <-time.After(5 * time.Second):
		t.Fatal("source never started its session")
	}
	for i := 1; i <= 2; i++ {
		err := publish(replayTopic, domain.StreamEvent{
			Source:  "replay-removed",
			Type:    "reading",
			Payload: json.RawMessage(fmt.Sprintf(`{"n":%d}`, i)),
		})
		if err != nil {
			t.Fatalf("publish %d: %v", i, err)
		}
		time.Sleep(publishSpacing)
	}

	if err := manager.Remove("replay-removed"); err != nil {
		t.Fatalf("remove: %v", err)
	}
	if _, ok := manager.StatusOf("replay-removed"); ok {
		t.Fatal("stream still registered after Remove")
	}

	nc, err := nats.Connect(srv.ClientURL())
	if err != nil {
		t.Fatalf("consumer connect: %v", err)
	}
	t.Cleanup(nc.Close)

	wantN(t, drain(t, pullAll(t, nc, nats.DeliverAll()), replayEventCount), []int{1, 2})
}

// TestReplayConsumerIsIndependentOfNephtys replays with the Nephtys manager
// already stopped, which is the state a real recovery runs in: the operator
// reads history back while the connector that produced it is not running.
// Nothing on the replay path goes through Nephtys, and this is what says so.
func TestReplayConsumerIsIndependentOfNephtys(t *testing.T) {
	manager, nc := setupReplayStream(t, "replay-detached")

	manager.StopAll()
	if status, ok := manager.StatusOf("replay-detached"); ok && status != domain.StatusStopped {
		t.Fatalf("stream is %s after StopAll, want stopped", status)
	}

	got := drain(t, pullAll(t, nc, nats.DeliverAll()), replayEventCount)
	wantN(t, got, []int{1, 2, 3, 4, 5, 6})
}
