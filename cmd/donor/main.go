package main

import (
	"flag"
	"fmt"
	"log"
	"log/slog"
	"strconv"
	"strings"

	"hirocrossclusterworkloads/internal/services/donor/controller"
	"hirocrossclusterworkloads/internal/services/donor/mutate"
	results "hirocrossclusterworkloads/internal/services/donor/results"
	natsconnect "hirocrossclusterworkloads/pkg/connector/nats"
	"hirocrossclusterworkloads/pkg/core/donor"
	"hirocrossclusterworkloads/pkg/metrics"

	"github.com/google/uuid"
	"github.com/spf13/viper"
)

var (
	tlsKeyPath  string
	tlsCertPath string
)

func main() {
	stopChan := make(chan bool)

	// Start Prometheus metrics server
	mPath := "/" + getENVValue("METRICS_PATH").(string)
	mPort := ":" + strconv.Itoa(getENVValue("METRICS_PORT").(int))
	metrics.StartDonorMetricsServer(mPath, mPort)

	// To Do: Every restrat of the donor will have a new UUID.
	// This logic has to be changed so that the UUID is persisted.
	donorUUID := uuid.New().String()
	// donorUUID := "b458a190-4744-46f4-b16b-2739cf9fccb8"

	slog.Info("Configuring Validator")
	natsConfig := natsconnect.NATSClient{
		NATSURL:     getENVValue("NATS_URL").(string),
		NATSSubject: getENVValue("NATS_WORKLOAD_SUBJECT").(string),
	}
	validatorConfig := donor.DVConfig{
		Nconfig:            natsConfig,
		DonorUUID:          donorUUID,
		IgnoreNamespaces:   strings.Split(getENVValue("IGNORE_NAMESPACES").(string), ","),
		LableToFilter:      getENVValue("NO_WORK_LOAD_STEAL_LABLE").(string),
		WaitToGetPodStolen: getENVValue("WAIT_TIME_TO_GET_WORKLOAD_STOLEN_IN_MIN").(int),
	}
	validator, err := mutate.New(validatorConfig)
	if err != nil {
		log.Fatal(err)
	}

	slog.Info("Configuring Controller(Admission WebHook)")
	controllerConfig := donor.DSConfig{
		MPort:       8443,
		VPort:       8444,
		TLSKeyPath:  tlsKeyPath,
		TLSCertPath: tlsCertPath,
	}
	server := controller.New(controllerConfig, validator)

	slog.Info("Configuring Worker")
	workerNATSConfig := natsconnect.NATSClient{
		NATSURL:     getENVValue("NATS_URL").(string),
		NATSSubject: getENVValue("NATS_RETURN_WORKLOAD_SUBJECT").(string),
	}
	workerConfig := donor.DWConfig{
		Nclient:   workerNATSConfig,
		DonorUUID: donorUUID,
	}
	consumer, err := results.New(workerConfig)
	if err != nil {
		log.Fatal(err)
	}
	//go log.Fatal(server.Start(stopChan))
	go func() {
		server.StartMutate(stopChan)
	}()

	go func() {
		server.StartValidate(stopChan)
	}()

	go func() {
		consumer.Start(stopChan)
	}()

	<-stopChan
}

func init() {
	flag.StringVar(&tlsKeyPath, "tlsKeyPath", "/etc/certs/tls.key", "Absolute path to the TLS key")
	flag.StringVar(&tlsCertPath, "tlsCertPath", "/etc/certs/tls.crt", "Absolute path to the TLS certificate")

	flag.Parse()
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
		intValue, err := strconv.Atoi(valueStr)
		if err == nil {
			return intValue
		}
		return valueStr
	}
	return valueInt
}
