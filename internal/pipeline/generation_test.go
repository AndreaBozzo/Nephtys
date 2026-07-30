package pipeline

import (
	"encoding/json"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"nephtys/internal/domain"
)

// countingSink records every payload that reaches the end of a chain, including
// the ones a batch middleware aggregates into an array.
type countingSink struct {
	mu    sync.Mutex
	ids   []float64
	calls int
}

func (s *countingSink) handler(topic string, e domain.StreamEvent) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.calls++

	// A batched event carries a JSON array of the original payloads; an
	// unbatched one carries a single payload. Both count as delivered.
	var batched []map[string]float64
	if err := json.Unmarshal(e.Payload, &batched); err == nil {
		for _, p := range batched {
			s.ids = append(s.ids, p["id"])
		}
		return nil
	}
	var single map[string]float64
	if err := json.Unmarshal(e.Payload, &single); err != nil {
		return err
	}
	s.ids = append(s.ids, single["id"])
	return nil
}

func (s *countingSink) delivered() []float64 {
	s.mu.Lock()
	defer s.mu.Unlock()
	out := make([]float64, len(s.ids))
	copy(out, s.ids)
	return out
}

func event(id int) domain.StreamEvent {
	return domain.StreamEvent{
		Source:  "test",
		Type:    "evt",
		Payload: json.RawMessage(`{"id":` + jsonInt(id) + `}`),
	}
}

func jsonInt(i int) string {
	b, _ := json.Marshal(i)
	return string(b)
}

// TestGeneration_LateEnqueueIsNotStranded is the regression test for #57.
//
// The hazard is a publisher that passes the batch middleware's cancellation
// check moments before the generation is retired, and lands its event in the
// buffer after the worker has drained. Nothing owns the event afterwards: it is
// never flushed and never reported, which is silent data loss.
//
// The test drives that ordering deterministically rather than hoping to hit it.
// A publisher is stopped inside the chain, mid-call, with its event not yet
// buffered; retirement is started while it sits there; the publisher is then
// released so it enqueues into a generation that is already retiring. The event
// must still be published.
func TestGeneration_LateEnqueueIsNotStranded(t *testing.T) {
	sink := &countingSink{}

	// A middleware in front of batch that can hold a publisher in place. This
	// is the "has passed the point of no return but has not yet enqueued"
	// position that the race needs and that wall-clock timing cannot pin down.
	held := make(chan struct{})
	release := make(chan struct{})
	var holdOnce sync.Once
	hold := func(next Handler) Handler {
		return func(topic string, e domain.StreamEvent) error {
			holdOnce.Do(func() {
				close(held)
				<-release
			})
			return next(topic, e)
		}
	}

	gen := newGeneration()
	batch, err := NewBatch(gen, &domain.BatchConfig{
		Enabled: true, MaxBatchSize: 100, FlushInterval: "1h",
	})
	if err != nil {
		t.Fatalf("NewBatch: %v", err)
	}
	gen.handler = New(hold, batch).Execute(sink.handler)

	// The publisher enters the chain and parks inside `hold`, holding the
	// generation's in-flight lock.
	done := make(chan error, 1)
	go func() { done <- gen.Publish("topic", event(1)) }()
	<-held

	// Retirement starts while the publisher is parked. It must not get past
	// sealing — and therefore the worker must not drain — until the publisher
	// has left the chain.
	retired := make(chan struct{})
	go func() { defer close(retired); gen.Retire() }()

	// Give retirement every chance to run ahead. Before #57 this is the window
	// in which the worker drained an empty buffer and exited.
	<-gen.Done()
	time.Sleep(50 * time.Millisecond)

	select {
	case <-retired:
		t.Fatal("Retire completed while a publisher was still inside the chain")
	default:
	}

	// Release the publisher: it now enqueues into a generation being retired.
	close(release)

	if err := <-done; err != nil {
		t.Fatalf("late publisher returned %v, want nil", err)
	}
	<-retired

	got := sink.delivered()
	if len(got) != 1 || got[0] != 1 {
		t.Fatalf("delivered %v, want exactly [1] — the late event was stranded", got)
	}
}

// TestGeneration_WorkerDoesNotDrainBeforeSealed is the other half of the #57
// regression, and the one the in-flight lock alone does not provide.
//
// Waiting for publishers to leave is not enough on its own: if the worker
// drains the moment the generation is cancelled, it can empty and abandon the
// buffer while a publisher is still inside the chain on its way to enqueueing.
// Sealing is what orders those two, so the observable contract is that no flush
// happens between cancellation and the last publisher leaving.
func TestGeneration_WorkerDoesNotDrainBeforeSealed(t *testing.T) {
	sink := &countingSink{}

	// Park the *second* publisher, so the first can buffer an event that only
	// the retirement drain will flush.
	var calls atomic.Int32
	held := make(chan struct{})
	release := make(chan struct{})
	hold := func(next Handler) Handler {
		return func(topic string, e domain.StreamEvent) error {
			if calls.Add(1) == 2 {
				close(held)
				<-release
			}
			return next(topic, e)
		}
	}

	gen := newGeneration()
	batch, err := NewBatch(gen, &domain.BatchConfig{
		Enabled: true, MaxBatchSize: 100, FlushInterval: "1h",
	})
	if err != nil {
		t.Fatalf("NewBatch: %v", err)
	}
	gen.handler = New(hold, batch).Execute(sink.handler)

	// Buffered, and with a 1h flush interval nothing but the drain will emit it.
	if err := gen.Publish("topic", event(1)); err != nil {
		t.Fatalf("publish 1: %v", err)
	}

	second := make(chan error, 1)
	go func() { second <- gen.Publish("topic", event(2)) }()
	<-held

	retired := make(chan struct{})
	go func() { defer close(retired); gen.Retire() }()
	<-gen.Done()

	// The worker has seen cancellation. It must be waiting to be sealed, not
	// draining: a publisher is still inside the chain.
	time.Sleep(50 * time.Millisecond)
	if got := sink.delivered(); len(got) != 0 {
		t.Fatalf("worker flushed %v after cancellation while a publisher was still inside the chain", got)
	}

	close(release)
	if err := <-second; err != nil {
		t.Fatalf("second publisher returned %v, want nil", err)
	}
	<-retired

	if got := len(sink.delivered()); got != 2 {
		t.Errorf("published %d of 2 accepted events", got)
	}
}

// TestGeneration_RetireFlushesEverythingAccepted covers the ordinary case the
// late-enqueue test approaches from the other side: whatever a generation
// accepted is published by the time Retire returns, with no sleeping or polling
// in the assertion.
func TestGeneration_RetireFlushesEverythingAccepted(t *testing.T) {
	const total = 500
	sink := &countingSink{}

	gen, err := NewGeneration("retire-flush", &domain.PipelineConfig{
		// A long flush interval means nothing leaves on a timer: every event
		// that arrives has to be published by the retirement drain.
		Batch: &domain.BatchConfig{Enabled: true, MaxBatchSize: 1000, FlushInterval: "1h"},
	}, sink.handler)
	if err != nil {
		t.Fatalf("NewGeneration: %v", err)
	}

	for i := 0; i < total; i++ {
		if err := gen.Publish("topic", event(i)); err != nil {
			t.Fatalf("publish %d: %v", i, err)
		}
	}

	gen.Retire()

	if got := len(sink.delivered()); got != total {
		t.Errorf("published %d of %d accepted events", got, total)
	}
}

// TestGeneration_ConcurrentRetireIsIdempotent covers Remove and StopAll racing
// with a hot-swap: retiring the same generation from several goroutines must
// neither panic on a double close nor return before the drain is done.
func TestGeneration_ConcurrentRetireIsIdempotent(t *testing.T) {
	sink := &countingSink{}
	gen, err := NewGeneration("retire-race", &domain.PipelineConfig{
		Batch: &domain.BatchConfig{Enabled: true, MaxBatchSize: 1000, FlushInterval: "1h"},
	}, sink.handler)
	if err != nil {
		t.Fatalf("NewGeneration: %v", err)
	}

	for i := 0; i < 10; i++ {
		if err := gen.Publish("topic", event(i)); err != nil {
			t.Fatalf("publish %d: %v", i, err)
		}
	}

	var wg sync.WaitGroup
	for i := 0; i < 8; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			gen.Retire()
		}()
	}
	wg.Wait()

	// Every caller of Retire must observe a completed drain, not just the one
	// that won the Once.
	if got := len(sink.delivered()); got != 10 {
		t.Errorf("published %d of 10 accepted events", got)
	}
}

// TestGeneration_PublishAfterRetireForwardsDownstream pins the behaviour a
// hot-swap depends on: a publisher still holding the outgoing generation gets
// its event through rather than an error, even once retirement has finished.
func TestGeneration_PublishAfterRetireForwardsDownstream(t *testing.T) {
	sink := &countingSink{}
	gen, err := NewGeneration("publish-after-retire", &domain.PipelineConfig{
		Batch: &domain.BatchConfig{Enabled: true, MaxBatchSize: 100, FlushInterval: "1h"},
	}, sink.handler)
	if err != nil {
		t.Fatalf("NewGeneration: %v", err)
	}

	gen.Retire()

	if err := gen.Publish("topic", event(7)); err != nil {
		t.Fatalf("publish after retire returned %v, want nil", err)
	}

	got := sink.delivered()
	if len(got) != 1 || got[0] != 7 {
		t.Fatalf("delivered %v, want [7]", got)
	}
}

// TestGeneration_RetireReleasesParkedPublishers covers the load case: with a
// full buffer, publishers park inside the chain. Retirement has to release them
// before it can wait for them, or it would be waiting on itself.
func TestGeneration_RetireReleasesParkedPublishers(t *testing.T) {
	sink := &countingSink{}
	wedge := make(chan struct{})
	var wedged atomic.Bool

	gen := newGeneration()
	batch, err := NewBatch(gen, &domain.BatchConfig{
		Enabled: true, MaxBatchSize: 1, FlushInterval: "1h",
	})
	if err != nil {
		t.Fatalf("NewBatch: %v", err)
	}
	gen.handler = New(batch).Execute(func(topic string, e domain.StreamEvent) error {
		// Hold the worker inside its first flush so the one-slot buffer stays
		// full and later publishers have nowhere to go.
		if wedged.CompareAndSwap(false, true) {
			<-wedge
		}
		return sink.handler(topic, e)
	})

	// Fill the worker and then the buffer.
	if err := gen.Publish("topic", event(1)); err != nil {
		t.Fatalf("publish 1: %v", err)
	}
	if err := gen.Publish("topic", event(2)); err != nil {
		t.Fatalf("publish 2: %v", err)
	}

	parked := make(chan error, 4)
	for i := 3; i <= 6; i++ {
		go func(id int) { parked <- gen.Publish("topic", event(id)) }(i)
	}
	time.Sleep(50 * time.Millisecond) // let them park on the full buffer

	retired := make(chan struct{})
	go func() { defer close(retired); gen.Retire() }()

	for i := 0; i < 4; i++ {
		select {
		case err := <-parked:
			if err != nil {
				t.Errorf("parked publisher returned %v, want nil", err)
			}
		case <-time.After(2 * time.Second):
			t.Fatal("retirement did not release the parked publishers")
		}
	}

	close(wedge)
	select {
	case <-retired:
	case <-time.After(2 * time.Second):
		t.Fatal("Retire did not return after the worker drained")
	}

	if got := len(sink.delivered()); got != 6 {
		t.Errorf("published %d of 6 accepted events", got)
	}
}
