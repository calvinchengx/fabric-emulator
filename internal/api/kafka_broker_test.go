package api

import (
	"errors"
	"testing"
	"time"

	"github.com/segmentio/kafka-go"
)

func TestAlreadyExistsRecognisesKafkaError(t *testing.T) {
	if alreadyExists(errors.New("nope")) {
		t.Fatal("plain error was treated as TopicAlreadyExists")
	}
	if !alreadyExists(kafka.TopicAlreadyExists) {
		t.Fatal("TopicAlreadyExists was not recognised")
	}
}

func TestKafkaBrokerUnreachableFailsLoud(t *testing.T) {
	k := &kafkaBroker{bootstrap: "127.0.0.1:1"}
	if err := k.CreateTopic("t"); err == nil {
		t.Fatal("CreateTopic succeeded against a closed port")
	}
	if err := k.Produce("t", []byte("k"), []byte("v")); err == nil {
		t.Fatal("Produce succeeded against a closed port")
	}
	recs, err := k.Consume("t", 0, 200*time.Millisecond)
	if err == nil && len(recs) > 0 {
		t.Fatal("Consume returned records from a closed port")
	}
}
