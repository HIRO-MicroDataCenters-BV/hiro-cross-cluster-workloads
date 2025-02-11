package stealer

import (
	natsconnect "hirocrossclusterworkloads/pkg/connector/nats"
)

type SWConfig struct {
	Nclient     natsconnect.NATSClient
	StealerUUID string
}

type SIConfig struct {
	Nclient          natsconnect.NATSClient
	StealerUUID      string
	IgnoreNamespaces []string
}

type Stealer interface {
	Start(stopChan chan<- bool) error
}
