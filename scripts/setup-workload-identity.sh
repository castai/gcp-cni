#!/bin/bash
set -e

# Configuration
PROJECT_ID="${PROJECT_ID:-}"
GCP_SA_NAME="${GCP_SA_NAME:-adam-gcp-cni}"
GCP_SA_EMAIL="${GCP_SA_NAME}@${PROJECT_ID}.iam.gserviceaccount.com"
K8S_NAMESPACE="kube-system"
K8S_SA_NAME="gcp-cni-provisioner"

echo "================================================"
echo "GCP CNI Workload Identity Setup"
echo "================================================"
echo "Project ID:           ${PROJECT_ID}"
echo "GCP Service Account:  ${GCP_SA_EMAIL}"
echo "K8s Namespace:        ${K8S_NAMESPACE}"
echo "K8s Service Account:  ${K8S_SA_NAME}"
echo "================================================"
echo ""

# Check if gcloud is authenticated
if ! gcloud auth list --filter=status:ACTIVE --format="value(account)" &>/dev/null; then
    echo "Error: Not authenticated with gcloud. Run 'gcloud auth login' first."
    exit 1
fi

echo "Step 1: Granting IAM roles to GCP service account..."
gcloud projects add-iam-policy-binding "${PROJECT_ID}" \
    --member="serviceAccount:${GCP_SA_EMAIL}" \
    --role="roles/compute.networkAdmin" \
    --condition=None

echo ""
echo "Step 2: Binding Kubernetes SA to GCP SA for Workload Identity..."
gcloud iam service-accounts add-iam-policy-binding "${GCP_SA_EMAIL}" \
    --role roles/iam.workloadIdentityUser \
    --member "serviceAccount:${PROJECT_ID}.svc.id.goog[${K8S_NAMESPACE}/${K8S_SA_NAME}]"

echo ""
echo "================================================"
echo "✅ Workload Identity setup completed!"
echo "================================================"
echo ""
echo "The GCP service account now has:"
echo "  - roles/compute.networkAdmin (for managing networks and addresses)"
echo ""
echo "The binding allows:"
echo "  - ${K8S_NAMESPACE}/${K8S_SA_NAME} to impersonate ${GCP_SA_EMAIL}"
echo ""
echo "Next steps:"
echo "  1. Delete the old job: kubectl delete job gcp-cni-provisioner -n kube-system"
echo "  2. Reapply the manifest: kubectl apply -f deploy/provisioner.yaml"
echo "  3. Check logs: kubectl logs -n kube-system job/gcp-cni-provisioner -f"
echo "================================================"
