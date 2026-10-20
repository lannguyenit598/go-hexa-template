package kafka_test

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"errors"
	"testing"
	"time"

	"github.com/twmb/franz-go/pkg/kfake"
	"github.com/twmb/franz-go/pkg/kgo"

	"github.com/example/userapi/internal/adapter/outbound/kafka"
	"github.com/example/userapi/internal/outbox"
)

// fakeCluster runs an in-memory Kafka cluster (kfake); no real broker is needed.
func fakeCluster(t *testing.T, topic string) []string {
	t.Helper()
	c, err := kfake.NewCluster(kfake.NumBrokers(1), kfake.SeedTopics(3, topic))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(c.Close)
	return c.ListenAddrs()
}

func consumeAll(t *testing.T, brokers []string, topic string, want int) []*kgo.Record {
	t.Helper()
	cl, err := kgo.NewClient(kgo.SeedBrokers(brokers...), kgo.ConsumeTopics(topic), kgo.ConsumeResetOffset(kgo.NewOffset().AtStart()))
	if err != nil {
		t.Fatal(err)
	}
	defer cl.Close()
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	var out []*kgo.Record
	for len(out) < want {
		fetches := cl.PollFetches(ctx)
		if err := ctx.Err(); err != nil {
			t.Fatalf("consumed %d of %d records: %v", len(out), want, err)
		}
		fetches.EachRecord(func(r *kgo.Record) { out = append(out, r) })
	}
	return out
}

func header(r *kgo.Record, key string) string {
	for _, h := range r.Headers {
		if h.Key == key {
			return string(h.Value)
		}
	}
	return ""
}

func TestSink_PublishesWithKeyAndHeaders(t *testing.T) {
	brokers := fakeCluster(t, "users")
	sink, err := kafka.NewSink(brokers, "users", kafka.Options{})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(sink.Close)
	ctx := context.Background()
	if err := sink.Ping(ctx); err != nil {
		t.Fatal(err)
	}

	msgs := []outbox.Message{
		{ID: "e1", Key: "u1", Type: "user.registered", Payload: []byte(`{"v":1}`), Headers: map[string]string{"traceparent": "00-abc-def-01"}},
		{ID: "e2", Key: "u1", Type: "user.updated", Payload: []byte(`{"v":2}`)},
		{ID: "e3", Key: "u2", Type: "user.registered", Payload: []byte(`{"v":1}`)},
	}
	for i, err := range sink.Publish(ctx, msgs) {
		if err != nil {
			t.Fatalf("message %d: %v", i, err)
		}
	}

	recs := consumeAll(t, brokers, "users", len(msgs))
	byID := map[string]*kgo.Record{}
	partitionOf := map[string]int32{}
	for _, r := range recs {
		byID[header(r, "event_id")] = r
		if p, ok := partitionOf[string(r.Key)]; ok && p != r.Partition {
			t.Fatalf("key %s landed on partitions %d and %d; per-user order needs one", r.Key, p, r.Partition)
		}
		partitionOf[string(r.Key)] = r.Partition
	}
	for _, m := range msgs {
		r := byID[m.ID]
		if r == nil {
			t.Fatalf("event %s not delivered", m.ID)
		}
		if string(r.Key) != m.Key || string(r.Value) != string(m.Payload) || header(r, "event_type") != m.Type {
			t.Fatalf("record for %s = key %q value %q type %q", m.ID, r.Key, r.Value, header(r, "event_type"))
		}
	}
	if got := header(byID["e1"], "traceparent"); got != "00-abc-def-01" {
		t.Fatalf("traceparent = %q", got)
	}
	// Records with the same key retain write order.
	u1 := []string{}
	for _, r := range recs {
		if string(r.Key) == "u1" {
			u1 = append(u1, header(r, "event_id"))
		}
	}
	if len(u1) != 2 || u1[0] != "e1" || u1[1] != "e2" {
		t.Fatalf("order for key u1 = %v, want [e1 e2]", u1)
	}
}

// An unreachable broker must make Publish return within the context deadline, not hang indefinitely.
func TestSink_PublishFailsWhenBrokerIsDown(t *testing.T) {
	sink, err := kafka.NewSink([]string{"127.0.0.1:1"}, "users", kafka.Options{}) // Nothing is listening here.
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(sink.Close)
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	errs := sink.Publish(ctx, []outbox.Message{{ID: "e1", Key: "u1", Payload: []byte(`{}`)}})
	if len(errs) != 1 || errs[0] == nil {
		t.Fatalf("Publish without a broker = %v, want an error", errs)
	}
	// An unreachable broker is temporary: retry the message rather than parking it.
	if errors.Is(errs[0], outbox.ErrPermanent) {
		t.Fatalf("broker down classified as permanent: %v", errs[0])
	}
}

// Content rejection (here, exceeding message.max.bytes) is permanent and must not
// affect the other records in the batch.
func TestSink_ClassifiesRejectedRecordsAsPermanent(t *testing.T) {
	c, err := kfake.NewCluster(kfake.NumBrokers(1), kfake.SeedTopics(1, "users"),
		kfake.BrokerConfigs(map[string]string{"message.max.bytes": "3000"}))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(c.Close)
	sink, err := kafka.NewSink(c.ListenAddrs(), "users", kafka.Options{})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(sink.Close)

	// Random data prevents compression below the limit. All three records share a partition and
	// usually a batch: the broker rejects the batch, and the sink must identify the offending record.
	random := make([]byte, 6000)
	if _, err := rand.Read(random); err != nil {
		t.Fatal(err)
	}
	big := []byte(`{"pad":"` + hex.EncodeToString(random) + `"}`)
	errs := sink.Publish(context.Background(), []outbox.Message{
		{ID: "ok1", Key: "u1", Payload: []byte(`{}`)},
		{ID: "big", Key: "u2", Payload: big},
		{ID: "ok2", Key: "u3", Payload: []byte(`{}`)},
	})
	if len(errs) != 3 {
		t.Fatalf("got %d results for 3 messages", len(errs))
	}
	if errs[0] != nil || errs[2] != nil {
		t.Fatalf("small messages failed: %v", errs)
	}
	if !errors.Is(errs[1], outbox.ErrPermanent) {
		t.Fatalf("oversized message error = %v, want ErrPermanent", errs[1])
	}
	if recs := consumeAll(t, c.ListenAddrs(), "users", 2); len(recs) != 2 {
		t.Fatalf("delivered %d records, want 2", len(recs))
	}
}
