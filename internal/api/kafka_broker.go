package api

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net"
	"strconv"
	"time"

	"github.com/segmentio/kafka-go"
)

// KafkaRecord is one Kafka message in the shape Spark's kafka source exposes.
type KafkaRecord struct {
	Key           []byte    `json:"key"`
	Value         []byte    `json:"value"`
	Topic         string    `json:"topic"`
	Partition     int       `json:"partition"`
	Offset        int64     `json:"offset"`
	Timestamp     time.Time `json:"timestamp"`
	TimestampType int       `json:"timestampType"`
}

// KafkaBroker provisions topics and produces/consumes records for Eventstream exec.
type KafkaBroker interface {
	CreateTopic(topic string) error
	Produce(topic string, key, value []byte) error
	Consume(topic string, max int, wait time.Duration) ([]KafkaRecord, error)
}

// SetKafkaBootstrap attaches an Apache Kafka broker (empty detaches it).
func (a *API) SetKafkaBootstrap(raw string) error {
	if raw == "" {
		a.KafkaBootstrap = ""
		a.Kafka = nil
		return nil
	}
	host, port, err := net.SplitHostPort(raw)
	if err != nil || host == "" || port == "" {
		return fmt.Errorf("invalid Kafka bootstrap %q (want host:port)", raw)
	}
	a.KafkaBootstrap = net.JoinHostPort(host, port)
	a.Kafka = &kafkaBroker{bootstrap: a.KafkaBootstrap}
	return nil
}

type kafkaBroker struct {
	bootstrap string
}

func (k *kafkaBroker) CreateTopic(topic string) error {
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	conn, err := kafka.DialContext(ctx, "tcp", k.bootstrap)
	if err != nil {
		return err
	}
	defer func() { _ = conn.Close() }()
	controller, err := conn.Controller()
	if err != nil {
		return err
	}
	ctrl, err := kafka.DialContext(ctx, "tcp", net.JoinHostPort(controller.Host, strconv.Itoa(controller.Port)))
	if err != nil {
		return err
	}
	defer func() { _ = ctrl.Close() }()
	err = ctrl.CreateTopics(kafka.TopicConfig{
		Topic:             topic,
		NumPartitions:     1,
		ReplicationFactor: 1,
	})
	if err != nil && !alreadyExists(err) {
		return err
	}
	return nil
}

func (k *kafkaBroker) Produce(topic string, key, value []byte) error {
	w := &kafka.Writer{
		Addr:         kafka.TCP(k.bootstrap),
		Topic:        topic,
		Balancer:     &kafka.LeastBytes{},
		RequiredAcks: kafka.RequireOne,
		BatchTimeout: 10 * time.Millisecond,
	}
	defer func() { _ = w.Close() }()
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	return w.WriteMessages(ctx, kafka.Message{Key: key, Value: value})
}

func (k *kafkaBroker) Consume(topic string, max int, wait time.Duration) ([]KafkaRecord, error) {
	if max <= 0 {
		max = 100
	}
	if wait <= 0 {
		wait = 5 * time.Second
	}
	r := kafka.NewReader(kafka.ReaderConfig{
		Brokers:     []string{k.bootstrap},
		Topic:       topic,
		Partition:   0,
		MinBytes:    1,
		MaxBytes:    int(10e6),
		MaxWait:     200 * time.Millisecond,
		StartOffset: kafka.FirstOffset,
	})
	defer func() { _ = r.Close() }()

	ctx, cancel := context.WithTimeout(context.Background(), wait)
	defer cancel()

	recs := []KafkaRecord{}
	for len(recs) < max {
		msg, err := r.FetchMessage(ctx)
		if err != nil {
			if errors.Is(err, context.DeadlineExceeded) || errors.Is(err, io.EOF) {
				break
			}
			if len(recs) > 0 {
				return recs, nil
			}
			return nil, err
		}
		recs = append(recs, KafkaRecord{
			Key:           msg.Key,
			Value:         msg.Value,
			Topic:         msg.Topic,
			Partition:     msg.Partition,
			Offset:        msg.Offset,
			Timestamp:     msg.Time,
			TimestampType: 0,
		})
	}
	return recs, nil
}

func alreadyExists(err error) bool {
	var ke kafka.Error
	if errors.As(err, &ke) && ke == kafka.TopicAlreadyExists {
		return true
	}
	return false
}
