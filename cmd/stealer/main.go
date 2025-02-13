package main

import (
	"fmt"
	"log"
	"log/slog"
	"strings"

	"hirocrossclusterworkloads/internal/services/stealer/informer"
	"hirocrossclusterworkloads/internal/services/stealer/worker"
	natsconnect "hirocrossclusterworkloads/pkg/connector/nats"
	"hirocrossclusterworkloads/pkg/core/stealer"

	"github.com/google/uuid"
	"github.com/spf13/viper"
)

func main() {
	stopChan := make(chan bool)

	// To Do: Every restrat of the worker will have a new UUID.
	// This logic has to be changed so that the UUID will never change.
	stealerUUID := uuid.New().String()

	slog.Info("Configuring Worker")
	natsConfig := natsconnect.NATSClient{
		NATSURL:     getENVValue("NATS_URL").(string),
		NATSSubject: getENVValue("NATS_WORKLOAD_SUBJECT").(string),
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
		NATSURL:     getENVValue("NATS_URL").(string),
		NATSSubject: getENVValue("NATS_RETURN_WORKLOAD_SUBJECT").(string),
	}
	informerConfig := stealer.SIConfig{
		Nclient:          informerNATSConfig,
		StealerUUID:      stealerUUID,
		IgnoreNamespaces: strings.Split(getENVValue("IGNORE_NAMESPACES").(string), ","),
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

func getENVValue(envKey string) any {
	// Read environment variables
	valueStr := viper.GetString(envKey)
	valueInt := viper.GetInt(envKey)
	if valueStr == "" && valueInt == 0 {
		message := fmt.Sprintf("%s environment variable is not set", envKey)
		log.Fatal(message)
	}
	if valueStr != "" {
		return valueStr
	}
	return valueInt
}
