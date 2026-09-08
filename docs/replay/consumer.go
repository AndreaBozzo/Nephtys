// consumer — example JetStream consumer that replays events Nephtys published.
//
// It is the runnable companion to docs/REPLAY.md: one file, no dependencies
// beyond the NATS client, and it does the one thing the `nats` CLI cannot —
// start a replay at an absolute wall-clock instant.
//
// Usage:
//
//	go run docs/replay/consumer.go [flags]
//
//	-from all                   every event still retained (default)
//	-from new                   only events published from now on
//	-from last                  the newest event, then everything after it
//	-from last-per-subject      the newest event of each subject
//	-from 4210                  stream sequence 4210 and everything after
//	-from 2h                    everything from two hours ago
//	-from 2026-09-08T06:00:00Z  everything from an absolute instant (RFC 3339)
//
// Examples:
//
//	# Backfill one sensor subject from six hours ago, and stop at the end.
//	go run docs/replay/consumer.go -subject "nephtys.stream.sensors.>" -from 6h
//
//	# Resume where this consumer last acknowledged, and keep following.
//	go run docs/replay/consumer.go -durable warehouse-loader -idle 0
//
// The event envelope is redeclared here rather than imported from
// nephtys/internal/domain, so this file still compiles when copied out of the
// repository. The JSON field names are published API surface and only ever grow.
package main

import (
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"log"
	"os"
	"os/signal"
	"strconv"
	"strings"
	"syscall"
	"time"
	"unicode/utf8"

	"github.com/nats-io/nats.go"
)

// event mirrors nephtys/internal/domain.StreamEvent for JSON-encoded events.
// Events whose content_type is not application/json carry their bytes as the
// message body instead of inside this envelope, so they do not unmarshal into
// it — see printMessage.
type event struct {
	Source      string          `json:"source"`
	Type        string          `json:"type"`
	Timestamp   int64           `json:"timestamp"`
	Seq         int64           `json:"seq,omitempty"`
	ContentType string          `json:"content_type,omitempty"`
	Payload     json.RawMessage `json:"payload,omitempty"`
}

const contentTypeJSON = "application/json"

// payloadPreview caps how much of an event body is echoed per line.
const payloadPreview = 160

// fetchBatch is how many events are asked for per round trip. A pull consumer
// is used rather than a push one precisely because of this: the server sends
// only what has been asked for, so a consumer that stops mid-replay leaves at
// most one batch unacknowledged and the next run resumes immediately. A push
// consumer streams ahead of the acknowledgements, and everything it had already
// delivered is stuck until AckWait expires — thirty seconds, by default, in
// which a restarted consumer looks like it has lost the events.
const fetchBatch = 64

// flushTimeout bounds the wait for the last acknowledgements to reach the
// broker before the connection closes.
const flushTimeout = 5 * time.Second

func main() {
	var (
		server  = flag.String("server", envOr("NATS_URL", nats.DefaultURL), "NATS server URL.")
		stream  = flag.String("stream", "NEPHTYS", "JetStream stream holding Nephtys events.")
		subject = flag.String("subject", "nephtys.stream.>", "Subject filter within the stream.")
		from    = flag.String("from", "all", "Where to start: all, new, last, last-per-subject, a stream sequence, a duration ago (2h), or an RFC 3339 instant.")
		durable = flag.String("durable", "", "Durable consumer name. Resumes from the last acknowledged event, and ignores -from after the first run.")
		count   = flag.Int("count", 0, "Stop after this many events. 0 means no limit.")
		idle    = flag.Duration("idle", 5*time.Second, "Exit once no event arrives for this long — how a backfill knows it has reached the end. 0 waits forever.")
	)
	flag.Parse()

	if err := run(*server, *stream, *subject, *from, *durable, *count, *idle); err != nil {
		log.Fatalf("replay: %v", err)
	}
}

func run(server, stream, subject, from, durable string, count int, idle time.Duration) error {
	start, startDesc, err := startOption(from)
	if err != nil {
		return err
	}

	nc, err := nats.Connect(server)
	if err != nil {
		return fmt.Errorf("connect %s: %w", server, err)
	}
	defer nc.Close()

	js, err := nc.JetStream()
	if err != nil {
		return fmt.Errorf("jetstream: %w", err)
	}

	// BindStream removes the subject-to-stream lookup: the subject stays a
	// filter, and naming the stream is what stops a wildcard from resolving to
	// somebody else's stream. An empty durable name asks for an ephemeral
	// consumer, which the server discards once this process goes away.
	//
	// A durable consumer's start position is fixed when the server first
	// creates it. Every later run resumes from the last acknowledged event,
	// which is the point of it, and -from is inert from then on.
	sub, err := js.PullSubscribe(subject, durable, nats.BindStream(stream), start)
	if err != nil {
		return fmt.Errorf("subscribe %s on %s: %w", subject, stream, err)
	}

	// Deliberately no Drain or Unsubscribe on the way out. The NATS client
	// deletes the JetStream consumer behind any subscription it created when
	// that subscription is drained or unsubscribed — a durable one included —
	// which would throw away the acknowledgement floor a durable consumer
	// exists to keep, and turn every run into a replay from the start. Closing
	// the connection leaves the consumer where it is; an ephemeral one the
	// server reaps by itself once it goes idle.
	defer func() {
		// Acknowledgements are published without waiting for a reply, so the
		// last batch's are still in the client's buffer here. Flushing before
		// the connection closes is what makes the floor include them.
		if err := nc.FlushTimeout(flushTimeout); err != nil {
			fmt.Fprintf(os.Stderr, "warning: final acknowledgements may not have reached the broker: %v\n", err)
		}
	}()

	if durable == "" {
		fmt.Fprintf(os.Stderr, "replaying %s from %s\n", subject, startDesc)
	} else {
		fmt.Fprintf(os.Stderr, "replaying %s as durable %q, from its last acknowledged event (%s on first run)\n", subject, durable, startDesc)
	}

	// Interrupting is a normal way to end a follow, so SIGINT ends the loop and
	// still prints the summary rather than killing the process mid-line.
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	seen := 0
	for count == 0 || seen < count {
		want := fetchBatch
		if count > 0 && count-seen < want {
			want = count - seen
		}

		msgs, err := fetch(ctx, sub, want, idle)
		if err != nil {
			if errors.Is(err, context.Canceled) {
				fmt.Fprintln(os.Stderr, "interrupted")
				break
			}
			if quiet(err) {
				if idle <= 0 {
					// Following the stream: a round trip that brought nothing
					// back is a quiet interval, not the end of anything.
					continue
				}
				fmt.Fprintf(os.Stderr, "no event for %s — stopping\n", idle)
				break
			}
			return fmt.Errorf("receive: %w", err)
		}

		for _, msg := range msgs {
			printMessage(msg)
			seen++

			// Acknowledging is what makes a durable consumer resumable: an
			// event received but never acknowledged is redelivered on the next
			// run, which is the behaviour you want after a consumer crashes
			// part-way through a batch.
			if err := msg.Ack(); err != nil {
				return fmt.Errorf("ack: %w", err)
			}
		}
	}

	fmt.Fprintf(os.Stderr, "%d event(s) replayed\n", seen)
	return nil
}

// quiet reports whether an error means "nothing arrived in this round trip"
// rather than a failure.
//
// It is not only the idle deadline that produces one. The client bounds every
// Fetch with its own default wait — five seconds — even when the context it is
// handed carries no deadline of its own, so a consumer that follows a stream
// sees one of these on every quiet interval and has to keep going. Reading it
// as the end of the stream is what made `-idle 0` stop after five seconds
// instead of following.
func quiet(err error) bool {
	return errors.Is(err, context.DeadlineExceeded) || errors.Is(err, nats.ErrTimeout)
}

// fetch asks for up to want events, bounded by idle when idle is non-zero.
func fetch(ctx context.Context, sub *nats.Subscription, want int, idle time.Duration) ([]*nats.Msg, error) {
	if idle <= 0 {
		return sub.Fetch(want, nats.Context(ctx))
	}
	waitCtx, cancel := context.WithTimeout(ctx, idle)
	defer cancel()
	msgs, err := sub.Fetch(want, nats.Context(waitCtx))
	if err != nil && ctx.Err() != nil {
		// A cancelled parent reaches the child as a deadline; report the
		// interrupt rather than an idle timeout that has not elapsed.
		return nil, ctx.Err()
	}
	return msgs, err
}

// startOption translates -from into a JetStream delivery policy.
func startOption(from string) (nats.SubOpt, string, error) {
	switch strings.ToLower(strings.TrimSpace(from)) {
	case "all", "":
		return nats.DeliverAll(), "the oldest retained event", nil
	case "new":
		return nats.DeliverNew(), "the next event published", nil
	case "last":
		return nats.DeliverLast(), "the newest event", nil
	case "last-per-subject":
		return nats.DeliverLastPerSubject(), "the newest event of each subject", nil
	}

	// A bare number is a stream sequence. It is checked before durations so
	// that "100" is sequence 100 rather than 100 nanoseconds.
	if seq, err := strconv.ParseUint(from, 10, 64); err == nil {
		if seq == 0 {
			return nil, "", errors.New("-from: stream sequences start at 1")
		}
		return nats.StartSequence(seq), fmt.Sprintf("stream sequence %d", seq), nil
	}

	if d, err := time.ParseDuration(from); err == nil {
		if d < 0 {
			return nil, "", fmt.Errorf("-from %q: a duration is read as time ago, so it cannot be negative", from)
		}
		t := time.Now().Add(-d)
		return nats.StartTime(t), fmt.Sprintf("%s ago (%s)", d, t.Format(time.RFC3339)), nil
	}

	if t, err := time.Parse(time.RFC3339, from); err == nil {
		return nats.StartTime(t), t.Format(time.RFC3339), nil
	}

	return nil, "", fmt.Errorf("-from %q: expected all, new, last, last-per-subject, a stream sequence, a duration such as 2h, or an RFC 3339 instant", from)
}

// printMessage renders one replayed event as a single line: where it sits in
// the stream, when the broker stored it, and what it carries.
func printMessage(msg *nats.Msg) {
	meta, err := msg.Metadata()
	if err != nil {
		// Not a JetStream message: only reachable if the subject is also served
		// by something other than the stream this consumer is bound to.
		fmt.Printf("?\t%s\t%s\n", msg.Subject, preview(msg.Data))
		return
	}

	prefix := fmt.Sprintf("%d\t%s\t%s", meta.Sequence.Stream, meta.Timestamp.UTC().Format(time.RFC3339Nano), msg.Subject)

	contentType := msg.Header.Get("Content-Type")
	if contentType != "" && contentType != contentTypeJSON {
		// Binary events are published as raw bytes with no envelope around them.
		fmt.Printf("%s\t%s\t%d bytes\tX-Nephtys-Seq=%s\n", prefix, contentType, len(msg.Data), msg.Header.Get("X-Nephtys-Seq"))
		return
	}

	var ev event
	if err := json.Unmarshal(msg.Data, &ev); err != nil {
		fmt.Printf("%s\tunparsed\t%s\n", prefix, preview(msg.Data))
		return
	}
	fmt.Printf("%s\tsource=%s\ttype=%s\tseq=%d\t%s\n", prefix, ev.Source, ev.Type, ev.Seq, preview(ev.Payload))
}

func preview(b []byte) string {
	s := strings.TrimSpace(string(b))
	if len(s) <= payloadPreview {
		return s
	}
	// Back up to a rune boundary. A payload is arbitrary UTF-8, and cutting one
	// in half puts a replacement character in the output.
	cut := payloadPreview
	for cut > 0 && !utf8.RuneStart(s[cut]) {
		cut--
	}
	return s[:cut] + "…"
}

func envOr(key, fallback string) string {
	if v := os.Getenv(key); v != "" {
		return v
	}
	return fallback
}
