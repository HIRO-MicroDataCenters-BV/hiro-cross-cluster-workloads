#!/bin/bash
echo "Checking if subctl command is available..."
# Check if subctl command is available
if ! command -v subctl &> /dev/null
then
	echo "subctl command not found. Please install subctl from Submariner."
	exit 1
fi

# Set default value for cluster name
CLUSTER_NAME=${1}

# Check if cluster name is provided
if [ -z "$CLUSTER_NAME" ]; then
	echo "Usage: $0 <cluster_name>"
	exit 1
fi

# Check if broker-info.subm file exists
if [ ! -f broker-info.subm ]; then
	echo "broker-info.subm file not found. Please install the broker to generate this file."
	exit 1
fi

echo "***Join the cluster to the broker***"
subctl join broker-info.subm --context=kind-$CLUSTER_NAME --check-broker-certificate=false
subctl show all --context=kind-$CLUSTER_NAME || echo true
