#!/bin/bash

CLUSTER_NAME=${1:-donor}
echo "Build Docker image"
docker build -t workloaddonor:alpha1 -f docker/donor/Dockerfile .

echo "Set the kubectl context to $CLUSTER_NAME cluster"
kubectl cluster-info --context kind-$CLUSTER_NAME
kubectl config use-context kind-$CLUSTER_NAME

echo "Load Image to Kind cluster named '$CLUSTER_NAME'"
kind load docker-image --name $CLUSTER_NAME workloaddonor:alpha1

echo "Restarting Agent Deployment"
kubectl rollout restart deployment workload-donor -n hiro