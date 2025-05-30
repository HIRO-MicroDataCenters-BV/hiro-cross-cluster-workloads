#!/bin/bash

CLUSTER_NAME=${1:-donor}
POD_FILE=${2:-pod1}
echo "Deploy a sample pod to the $CLUSTER_NAME cluster"
kubectl config use-context kind-$CLUSTER_NAME
kubectl apply -f scripts/donor/k8s_manifests/$POD_FILE.yaml --context kind-$CLUSTER_NAME