package main

import (
	"fmt"
	"log"
	"log/slog"
	"strings"

	// "workloadstealworker/pkg/informer"
	// "workloadstealworker/pkg/worker"
	"hirocrossclusterworkloads/internal/services/stealer/informer"
	"hirocrossclusterworkloads/internal/services/stealer/worker"
	natsconnect "hirocrossclusterworkloads/pkg/connector/nats"
	"hirocrossclusterworkloads/pkg/core/stealer"

	"github.com/google/uuid"
	"github.com/spf13/viper"
)

func main() {
	stopChan := make(chan bool)

	// To Do: Every resrat of the worker will have a new UUID.
	// This logic has to be changed so that the UUID is persisted.
	stealerUUID := uuid.New().String()

	slog.Info("Configuring Worker")
	natsConfig := natsconnect.NATSClient{
		NATSURL:     getENVValue("NATS_URL"),
		NATSSubject: getENVValue("NATS_WORKLOAD_SUBJECT"),
	}
	workerConfig := stealer.SWConfig{
		Nclient:     natsConfig,
		StealerUUID: stealerUUID,
	}
	consumer, err := worker.New(workerConfig)
	if err != nil {
		log.Fatal(err)
	}

	slog.Info("Configuring Informer")
	informerNATSConfig := natsconnect.NATSClient{
		NATSURL:     getENVValue("NATS_URL"),
		NATSSubject: getENVValue("NATS_RETURN_WORKLOAD_SUBJECT"),
	}
	informerConfig := stealer.SIConfig{
		Nclient:          informerNATSConfig,
		StealerUUID:      stealerUUID,
		IgnoreNamespaces: strings.Split(getENVValue("IGNORE_NAMESPACES"), ","),
	}
	informer, err := informer.New(informerConfig)
	if err != nil {
		log.Fatal(err)
	}

	go func() {
		consumer.Start(stopChan)
	}()

	go func() {
		informer.Start(stopChan)
	}()
	<-stopChan
}

func init() {
	// Initialize Viper
	viper.SetConfigType("env") // Use environment variables
	viper.AutomaticEnv()       // Automatically read environment variables
}

func getENVValue(envKey string) string {
	// Read environment variables
	value := viper.GetString(envKey)
	if value == "" {
		message := fmt.Sprintf("%s environment variable is not set", envKey)
		log.Fatal(message)
	}
	return value
}
