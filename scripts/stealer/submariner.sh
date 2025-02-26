#!/bin/bash
echo "Checking if subctl command is available..."
# Check if subctl command is available
if ! command -v subctl &> /dev/null
then
	echo "subctl command not found. Please install subctl from Submariner."
	exit 1
fi

# Set default value for cluster name
CLUSTER_NAME=${1:-stealer}

echo "Delete and Install Submariner on Kind cluster named '$CLUSTER_NAME'"
subctl uninstall --context=kind-$CLUSTER_NAME --yes
subctl deploy-broker --context=kind-$CLUSTER_NAME --globalnet


echo "Get the broker-info by replacing 0.0.0.0 with the IP of the host machine"
# replace 0.0.0.0 with IP of host machine to make it accessible from outside
cat broker-info.subm | base64 --decode > broker-info-decoded.subm
HOST_IP=$(ifconfig | grep 'inet ' | grep -v '127.0.0.1' | awk '{print $2}')
sed -i '' "s/0.0.0.0/$HOST_IP/g" broker-info-decoded.subm
cat broker-info-decoded.subm | base64 > broker-info.subm

echo "Join the cluster to the broker"
subctl join broker-info.subm --context=kind-$CLUSTER_NAME --check-broker-certificate=false
subctl show all --context=kind-$CLUSTER_NAME || echo true
