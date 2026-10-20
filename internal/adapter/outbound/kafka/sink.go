// Package kafka là adapter gửi sự kiện từ outbox lên Kafka (hoặc broker tương thích giao thức
// Kafka: Redpanda, MSK, Confluent, Event Hubs) qua franz-go. Adapter không biết schema của sự
// kiện: payload và header được chuyển nguyên vẹn.
package kafka

import (
	"context"
	"fmt"
	"sort"
	"time"

	"github.com/twmb/franz-go/pkg/kgo"

	"github.com/example/userapi/internal/outbox"
)

// deliveryTimeout giới hạn thời gian franz-go tự thử lại một record: broker sập thì Publish
// trả lỗi (relay thử lại lô sau) thay vì treo và giữ khoá outbox mãi.
const deliveryTimeout = 20 * time.Second

// Sink hiện thực outbox.Sink.
type Sink struct {
	client *kgo.Client
}

var _ outbox.Sink = (*Sink)(nil)

// NewSink tạo producer gửi vào topic. franz-go mặc định bật idempotent producer và acks=all:
// broker không ghi trùng khi producer thử lại, và thứ tự trong một partition được giữ.
func NewSink(brokers []string, topic string) (*Sink, error) {
	client, err := kgo.NewClient(
		kgo.SeedBrokers(brokers...),
		kgo.DefaultProduceTopic(topic),
		kgo.RecordDeliveryTimeout(deliveryTimeout),
	)
	if err != nil {
		return nil, fmt.Errorf("kafka client: %w", err)
	}
	return &Sink{client: client}, nil
}

// Ping kiểm tra kết nối tới broker; dùng lúc khởi động để cấu hình sai lộ ra ngay.
func (s *Sink) Ping(ctx context.Context) error {
	if err := s.client.Ping(ctx); err != nil {
		return fmt.Errorf("kafka ping: %w", err)
	}
	return nil
}

// Publish gửi cả lô và chờ broker xác nhận mọi record. Key là ID của aggregate, nên mọi sự
// kiện của một user nằm trên cùng partition, theo đúng thứ tự trong lô.
func (s *Sink) Publish(ctx context.Context, msgs []outbox.Message) error {
	records := make([]*kgo.Record, len(msgs))
	for i, m := range msgs {
		headers := []kgo.RecordHeader{
			{Key: "event_id", Value: []byte(m.ID)},
			{Key: "event_type", Value: []byte(m.Type)},
		}
		keys := make([]string, 0, len(m.Headers))
		for k := range m.Headers {
			keys = append(keys, k)
		}
		sort.Strings(keys) // thứ tự header ổn định giữa các lần gửi
		for _, k := range keys {
			headers = append(headers, kgo.RecordHeader{Key: k, Value: []byte(m.Headers[k])})
		}
		records[i] = &kgo.Record{Key: []byte(m.Key), Value: m.Payload, Headers: headers}
	}
	if err := s.client.ProduceSync(ctx, records...).FirstErr(); err != nil {
		return fmt.Errorf("kafka produce: %w", err)
	}
	return nil
}

// Close đóng kết nối tới broker.
func (s *Sink) Close() { s.client.Close() }
