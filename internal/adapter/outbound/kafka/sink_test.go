package kafka_test

import (
	"context"
	"testing"
	"time"

	"github.com/twmb/franz-go/pkg/kfake"
	"github.com/twmb/franz-go/pkg/kgo"

	"github.com/example/userapi/internal/adapter/outbound/kafka"
	"github.com/example/userapi/internal/outbox"
)

// fakeCluster chạy một cluster Kafka in-memory (kfake): test không cần broker thật.
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
	sink, err := kafka.NewSink(brokers, "users")
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
	if err := sink.Publish(ctx, msgs); err != nil {
		t.Fatal(err)
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
	// Cùng key thì giữ thứ tự ghi.
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

// Broker không tới được: Publish phải trả lỗi trong thời hạn của ctx, không treo mãi.
func TestSink_PublishFailsWhenBrokerIsDown(t *testing.T) {
	sink, err := kafka.NewSink([]string{"127.0.0.1:1"}, "users") // không có gì lắng nghe
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(sink.Close)
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	if err := sink.Publish(ctx, []outbox.Message{{ID: "e1", Key: "u1", Payload: []byte(`{}`)}}); err == nil {
		t.Fatal("Publish succeeded without a broker")
	}
}
