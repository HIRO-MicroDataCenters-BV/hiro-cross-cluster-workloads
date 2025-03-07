package main

import (
	"fmt"
	"log"
	"log/slog"
	"strconv"
	"strings"

	"hirocrossclusterworkloads/internal/services/stealer/results"
	"hirocrossclusterworkloads/internal/services/stealer/worker"
	natsconnect "hirocrossclusterworkloads/pkg/connector/nats"
	"hirocrossclusterworkloads/pkg/core/common"
	"hirocrossclusterworkloads/pkg/core/stealer"
	"hirocrossclusterworkloads/pkg/metrics"

	"github.com/spf13/viper"
)

func main() {
	stopChan := make(chan bool)

	// Start Prometheus metrics server
	mPath := "/" + getENVValue("METRICS_PATH").(string)
	mPort := ":" + strconv.Itoa(getENVValue("METRICS_PORT").(int))
	metrics.StartStealerMetricsServer(mPath, mPort)

	// To Do: Every restrat of the worker will have a new UUID.
	// This logic has to be changed so that the UUID will never change.
	//stealerUUID := uuid.New().String()
	//stealerUUID := "8b5ff588-ef41-4c69-ab1b-7b03f1bfc0e0-stealer"
	stealerUUID, err := common.GetClusterID()
	if err != nil || stealerUUID == "" {
		log.Fatal("Failed to get cluster ID", stealerUUID, err)
	}

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
	informer, err := results.New(informerConfig)
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
		// Try to convert the string to an integer
		if intValue, err := strconv.Atoi(valueStr); err == nil {
			return intValue
		}
		return valueStr
	}
	return valueInt
}
