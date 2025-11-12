# POC for GCP CNI allowing IP Migration

Usage:
1. Deploy a GKE cluster with:
- Workload Identity enabled
- Node Pools SA with following IAM roles:
  - roles/compute.networkAdmin
  - roles/compute.instanceAdmin.v1
  - roles/container.defaultNodeServiceAccount
2. Setup SA bindings for Workload Identity:
```bash
./scripts/setup-workload-identity.sh
```
3. Deploy GCP CNI with IP Migration enabled:
```bash
make docker-build-all docker-push-all helm-install
```

