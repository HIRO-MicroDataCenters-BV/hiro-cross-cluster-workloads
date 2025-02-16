package nats

import (
	"log/slog"

	"github.com/nats-io/nats.go"
)

var (
	JetStreamBucket = "message_tracking"
	JetStreamQueue  = "worker-group"
)

type NATSClient struct {
	NATSURL     string
	NATSSubject string
}

func (ns *NATSClient) GetJetStreamName() string {
	return "Stream" + ns.NATSSubject
}

func (ns *NATSClient) GetJetStreamQueueName() string {
	return JetStreamQueue
}

func (ns *NATSClient) GetJetStreamBucketName() string {
	return JetStreamBucket
}

func (ns *NATSClient) FetchNATSConnect() (*nats.Conn, error) {
	// Connect to NATS server
	natsConnect, err := nats.Connect(ns.NATSURL)
	if err != nil {
		slog.Error("Failed to connect to NATS server: ", "error", err)
		return nil, err
	}
	slog.Info("Connected to NATS server")
	return natsConnect, nil
}

func (ns *NATSClient) PublishNATSMessage(natsConnect *nats.Conn, message string) error {
	// Publish message to NATS server
	err := natsConnect.Publish(ns.NATSSubject, []byte(message))
	if err != nil {
		slog.Error("Failed to publish message to NATS server: ", "error", err)
		return err
	}
	slog.Info("Published message to NATS server")
	return nil
}

func (ns *NATSClient) PublishJSMessage(js nats.JetStreamContext, metadataJSON []byte) error {
	// Publish notification to JetStreams
	_, err := js.Publish(ns.NATSSubject, metadataJSON)
	if err != nil {
		slog.Error("Failed to publish message to NATS", "error", err, "subject", ns.NATSSubject, "metadataJSON", metadataJSON)
		return err
	}
	slog.Info("Published message to Jet Streams")
	return nil
}

func (ns *NATSClient) FetchJetStream(natsConnect *nats.Conn) (nats.JetStreamContext, error) {
	// Connect to JetStreams
	js, err := natsConnect.JetStream()
	if err != nil {
		slog.Error("Failed to connect to JetStreams server: ", "error", err)
		return nil, err
	}
	slog.Info("Connected to JetStreams server")
	return js, nil
}

func (ns *NATSClient) CreateJetStreamStream(js nats.JetStreamContext, streamName string) error {
	// Create a stream for message processing
	_, err := js.AddStream(&nats.StreamConfig{
		Name:      streamName,
		Subjects:  []string{ns.NATSSubject},
		Storage:   nats.FileStorage,
		Replicas:  1,
		Retention: nats.WorkQueuePolicy, // Ensures a message is only processed once
	})
	if err != nil {
		if err == nats.ErrStreamNameAlreadyInUse {
			slog.Info("JetStreams stream is already existed", "name", streamName, "subject", ns.NATSSubject)
			return nil
		} else {
			slog.Error("Failed to create JetStreams stream: ", "error", err)
			return err
		}
	}
	slog.Info("Created JetStreams stream", "name", streamName, "subject", ns.NATSSubject)
	return nil
}

func (ns *NATSClient) CreateOrGetKeyValueStore(js nats.JetStreamContext) (nats.KeyValue, error) {
	// Create a key-value store for message processing
	kv, err := js.KeyValue(JetStreamBucket)
	if err != nil && err != nats.ErrBucketNotFound {
		slog.Error("Failed to get KeyValue: ", "error", err)
		return nil, err
	}
	if kv == nil {
		kv, err = js.CreateKeyValue(&nats.KeyValueConfig{Bucket: JetStreamBucket})
		if err != nil {
			slog.Error("Failed to create KeyValue store: ", "error", err)
			return nil, err

		}
	}
	slog.Info("Created KeyValue store", "bucket", JetStreamBucket)
	return kv, nil
}

func (ns *NATSClient) GetNATSConnectJetStreamAndKeyValue() (*nats.Conn, nats.JetStreamContext, nats.KeyValue, error) {
	// Connect to NATS server
	natsConnect, err := nats.Connect(ns.NATSURL)
	if err != nil {
		slog.Error("Failed to connect to NATS server: ", "error", err)
		return nil, nil, nil, err
	}
	slog.Info("Connected to NATS server")

	// Connect to JetStreams
	js, err := ns.FetchJetStream(natsConnect)
	if err != nil {
		slog.Error("Failed to connect to JetStreams server: ", "error", err)
		return natsConnect, nil, nil, err
	}

	jsStreamName := ns.GetJetStreamName()
	// Create a stream for message processing
	ns.CreateJetStreamStream(js, jsStreamName)

	// Create or get the KV Store for message tracking
	kv, err := ns.CreateOrGetKeyValueStore(js)
	if err != nil {
		slog.Error("Failed to get KeyValue: ", "error", err)
		return natsConnect, js, nil, err
	}
	return natsConnect, js, kv, nil
}

func (ns *NATSClient) GetKeyValue(kv nats.KeyValue, key string) (string, error) {
	// Get key-value pair from store
	entry, err := kv.Get(key)
	if err != nil {
		slog.Error("Failed to get key-value pair from store: ", "error", err, "key", key)
		return "", err
	}
	slog.Info("Got key-value pair from store", "key", entry.Key(), "value", string(entry.Value()))
	return string(entry.Value()), nil
}

func (ns *NATSClient) PutKeyValue(kv nats.KeyValue, key string, value string) error {
	// Put key-value pair to store
	_, err := kv.Put(key, []byte(value))
	if err != nil {
		slog.Error("Failed to put key-value pair to store: ", "error", err, "key", key, "value", value)
		return err
	}
	slog.Info("Put key-value pair to store", "key", key, "value", value)
	return nil
}
