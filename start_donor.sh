#!/bin/bash

CLUSTER_NAME=${1:-donor}
# Run the initialize script
./scripts/donor/initialize.sh $CLUSTER_NAME

# Check if the initialize script ran successfully
if [ $? -ne 0 ]; then
    echo "Initialization failed. Exiting."
    exit 1
fi

# Install submariner
./scripts/donor/submariner.sh $CLUSTER_NAME

# Check if the submariner script ran successfully
if [ $? -ne 0 ]; then
    echo "Submariner installation failed. Exiting."
    exit 1
fi

# Run the install script
./scripts/donor/install.sh $CLUSTER_NAME

# Check if the install script ran successfully
if [ $? -ne 0 ]; then
    echo "Installation failed. Exiting."
    exit 1
fi

echo "All scripts ran successfully."