#!/bin/bash

# Set default value for cluster name
CLUSTER_NAME=${1}
IS_BROKER=${2:-false}

# Check if cluster name is provided
if [ -z "$CLUSTER_NAME" ] || [ -z "$IS_BROKER" ]; then
    echo "Usage: $0 <cluster_name> <is_broker:true/false>"
    exit 1
fi
# Run the initialize script
./scripts/stealer/initialize.sh $CLUSTER_NAME

# Check if the initialize script ran successfully
if [ $? -ne 0 ]; then
    echo "Initialization failed. Exiting."
    exit 1
fi

# Install submariner broker if the cluster is a broker
if [ "$IS_BROKER" = true ]; then
    ./scripts/submariner/submariner_broker.sh $CLUSTER_NAME
fi
./scripts/submariner/submariner_join.sh $CLUSTER_NAME

# Check if the submariner script ran successfully
if [ $? -ne 0 ]; then
    echo "Submariner installation failed. Exiting."
    exit 1
fi

# Run the install script
./scripts/stealer/install.sh $CLUSTER_NAME

# Check if the install script ran successfully
if [ $? -ne 0 ]; then
    echo "Installation failed. Exiting."
    exit 1
fi

echo "***All scripts ran successfully.***"