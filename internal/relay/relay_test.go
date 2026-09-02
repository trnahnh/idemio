package relay_test

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"os"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/segmentio/kafka-go"

	"github.com/trnahnh/idemio/internal/claim"
	"github.com/trnahnh/idemio/internal/manifest"
	"github.com/trnahnh/idemio/internal/relay"
	"github.com/trnahnh/idemio/internal/testdb"
)

const brokersEnv = "IDEMIO_TEST_KAFKA_BROKERS"

func brokers(t *testing.T) []string {
	t.Helper()

	raw := strings.TrimSpace(os.Getenv(brokersEnv))
	if raw == "" {
		t.Fatalf("%s is not set; start the stack with 'docker compose up -d' and export it. "+
			"Broker tests fail rather than skip: exit criterion 5 is about what happens to the "+
			"write path when the broker is unavailable, which is not provable against a stub alone.",
			brokersEnv)
	}
	return strings.Split(raw, ",")
}

func quiet() *slog.Logger { return slog.New(slog.NewJSONHandler(io.Discard, nil)) }

type recorder struct {
	mu       sync.Mutex
	messages []kafka.Message
	err      error
}

func (r *recorder) WriteMessages(_ context.Context, messages ...kafka.Message) error {
	r.mu.Lock()
	defer r.mu.Unlock()

	if r.err != nil {
		return r.err
	}
	r.messages = append(r.messages, messages...)
	return nil
}

func (r *recorder) count() int {
	r.mu.Lock()
	defer r.mu.Unlock()
	return len(r.messages)
}

func writeIntents(t *testing.T, pool *pgxpool.Pool, count int) {
	t.Helper()
	writeIntentsFrom(t, pool, 0, count)
}

func writeIntentsFrom(t *testing.T, pool *pgxpool.Pool, offset, count int) {
	t.Helper()

	for n := range count {
		i := offset + n
		_, err := claim.Claim(context.Background(), pool, claim.Request{
			AgentID:      "agent-checkout-flow",
			Key:          fmt.Sprintf("7c9e6679-7425-40de-944b-e07fc1f9%04d", i),
			RequestHash:  "sha256-jcs-v1:abc",
			ResourceType: "invoice",
			ResourceID:   fmt.Sprintf("inv_%d", i),
			Operation:    "create_charge",
			Declared:     manifest.Operation{Class: manifest.ClassCreate},
			Payload:      json.RawMessage(fmt.Sprintf(`{"amount_cents":%d}`, 100+i)),
			Window:       5 * time.Second,
			LockTimeout:  5 * time.Second,
		})
		if err != nil {
			t.Fatalf("claim %d: %v", i, err)
		}
	}
}

func unpublished(t *testing.T, pool *pgxpool.Pool) int {
	t.Helper()

	var count int
	if err := pool.QueryRow(context.Background(),
		"SELECT count(*) FROM write_intents WHERE published_at IS NULL").Scan(&count); err != nil {
		t.Fatalf("count unpublished: %v", err)
	}
	return count
}

func TestPublishingMarksTheOutboxAndDoesNotRepeat(t *testing.T) {
	pool := testdb.New(t)
	writeIntents(t, pool, 3)

	sink := &recorder{}
	r := relay.New(pool, sink, 500, quiet())

	sent, err := r.Publish(context.Background())
	if err != nil {
		t.Fatalf("publish: %v", err)
	}
	if sent != 3 || sink.count() != 3 {
		t.Fatalf("published %d, broker saw %d; want 3 and 3", sent, sink.count())
	}
	if got := unpublished(t, pool); got != 0 {
		t.Errorf("unpublished = %d, want 0", got)
	}

	again, err := r.Publish(context.Background())
	if err != nil {
		t.Fatalf("second publish: %v", err)
	}
	if again != 0 || sink.count() != 3 {
		t.Fatalf("republished %d messages; the watermark is not holding", again)
	}
}

// Publish then mark: a failure between the two republishes rather than dropping, which is
// what makes delivery at-least-once (ADR-0001).
func TestAFailedMarkLeavesTheIntentForTheNextCycle(t *testing.T) {
	pool := testdb.New(t)
	writeIntents(t, pool, 1)

	sink := &recorder{err: errors.New("broker unavailable")}
	if _, err := relay.New(pool, sink, 500, quiet()).Publish(context.Background()); err == nil {
		t.Fatal("publish reported success against a failing broker")
	}
	if got := unpublished(t, pool); got != 1 {
		t.Fatalf("unpublished = %d, want 1: an unpublished intent was marked as sent", got)
	}
}

// ROADMAP Phase 1 exit criterion 5. The broker is unreachable for the whole test and the
// write path neither fails nor slows: the relay is off the request path by construction.
func TestAnUnreachableBrokerDoesNotTouchTheWritePath(t *testing.T) {
	pool := testdb.New(t)

	dead := &kafka.Writer{
		Addr:         kafka.TCP("127.0.0.1:1"),
		Topic:        "idemio.write-intents",
		RequiredAcks: kafka.RequireAll,
		WriteTimeout: time.Second,
	}
	defer dead.Close()

	writeIntents(t, pool, 2)

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	if _, err := relay.New(pool, dead, 500, quiet()).Publish(ctx); err == nil {
		t.Fatal("publishing to a dead broker reported success")
	}

	writeIntentsFrom(t, pool, 2, 4)
	if got := unpublished(t, pool); got != 6 {
		t.Fatalf("unpublished = %d, want 6: writes did not continue while the broker was down", got)
	}
}

func TestIntentsReachTheBrokerAndCarryTheirPayload(t *testing.T) {
	pool := testdb.New(t)
	writeIntents(t, pool, 1)

	topic := fmt.Sprintf("idemio-test-%d", time.Now().UnixNano())
	writer := &kafka.Writer{
		Addr:                   kafka.TCP(brokers(t)...),
		Topic:                  topic,
		RequiredAcks:           kafka.RequireAll,
		AllowAutoTopicCreation: true,
	}
	defer writer.Close()

	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	if _, err := relay.New(pool, writer, 500, quiet()).Publish(ctx); err != nil {
		t.Fatalf("publish: %v", err)
	}

	reader := kafka.NewReader(kafka.ReaderConfig{
		Brokers:  brokers(t),
		Topic:    topic,
		MinBytes: 1,
		MaxBytes: 1 << 20,
	})
	defer reader.Close()

	received, err := reader.ReadMessage(ctx)
	if err != nil {
		t.Fatalf("read from broker: %v", err)
	}

	var published struct {
		IntentID string          `json:"intent_id"`
		Payload  json.RawMessage `json:"payload"`
	}
	if err := json.Unmarshal(received.Value, &published); err != nil {
		t.Fatalf("decode published intent: %v", err)
	}
	if published.IntentID == "" {
		t.Error("the published intent carries no identity")
	}
	if !strings.Contains(string(published.Payload), "amount_cents") {
		t.Errorf("payload = %s, want the original request body", published.Payload)
	}
}
