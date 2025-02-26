#!/bin/bash
# Set default value for cluster name
CLUSTER_NAME=${1:-stealer}
echo "Build Docker image"
docker build -t workloadstealer:alpha1 -f docker/stealer/Dockerfile .

echo "Set the kubectl context to $CLUSTER_NAME cluster"
kubectl cluster-info --context kind-$CLUSTER_NAME
kubectl config use-context kind-$CLUSTER_NAME

echo "Load Image to Kind cluster named '$CLUSTER_NAME'"
kind load docker-image --name $CLUSTER_NAME workloadstealer:alpha1

echo "Create 'hiro' namespace if it doesn't exist"
kubectl get namespace | grep -q "hiro" || kubectl create namespace hiro

echo "Deploying Worker Server"
kubectl apply -f deploy/stealer/deployment.yaml --context kind-$CLUSTER_NAME
