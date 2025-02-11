#!/bin/bash

CLUSTER_NAME=${1:-donor}
echo "Build Docker image"
docker build -t workloaddonor:alpha1 -f docker/donor/Dockerfile .

echo "Set the kubectl context to $CLUSTER_NAME cluster"
kubectl cluster-info --context kind-$CLUSTER_NAME

echo "Load Image to Kind cluster named '$CLUSTER_NAME'"
kind load docker-image --name $CLUSTER_NAME workloaddonor:alpha1

echo "Create 'hiro' namespace if it doesn't exist"
kubectl get namespace | grep -q "hiro" || kubectl create namespace hiro

echo "Creating certificates"
mkdir certs

openssl genrsa -out certs/tls.key 2048
openssl req -new -key certs/tls.key -out certs/tls.csr -subj "/CN=workload-donor.hiro.svc"
echo "subjectAltName=DNS:workload-donor.hiro.svc" > ./subj.txt
openssl x509 -req -extfile subj.txt -in certs/tls.csr -signkey certs/tls.key -out certs/tls.crt

echo "Creating Mutation Webhook Server TLS Secret in Kubernetes"
kubectl create secret tls workload-donor-tls \
    --cert "certs/tls.crt" \
    --key "certs/tls.key" -n hiro

echo "Deploying Webhook Server"
kubectl apply -f deploy/donor/deployment.yaml
kubectl apply -f deploy/donor/service.yaml

echo "Creating K8s Webhooks"
ENCODED_CA=$(cat certs/tls.crt | base64 | tr -d '\n')
sed -e "s/<ENCODED_CA>/${ENCODED_CA}/g" <"deploy/donor/webhook.yaml" | kubectl apply -f -
