package main

import (
	"time"

	memphis_kafka "github.com/memphisdev/memphis-kafka.go"

	"github.com/IBM/sarama"
)

func main() {

	broker := "..."
	config := sarama.NewConfig()
	config.Producer.Return.Successes = true
	config.Producer.Return.Errors = true
	config.Producer.Flush.MaxMessages = 10
	config.Producer.RequiredAcks = sarama.NoResponse

	// confluent config
	config.Net.SASL.Enable = true
	config.Net.SASL.User = "..."
	config.Net.SASL.Password = "..."
	config.Net.SASL.Mechanism = sarama.SASLTypePlaintext
	config.Net.TLS.Enable = true
	config.Net.TLS.Config = nil

	memphis_kafka.Init("token", config, memphis_kafka.Host("..."))

	producer, err := sarama.NewSyncProducer([]string{broker}, config)
	if err != nil {
		panic(err)
	}

	defer producer.Close()

	_, _, err = producer.SendMessage(&sarama.ProducerMessage{
		Topic: "test",
		Value: sarama.StringEncoder("test"),
	})

	if err != nil {
		panic(err)
	}

	time.Sleep(20 * time.Second)

}
