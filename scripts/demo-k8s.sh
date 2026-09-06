#!/usr/bin/env bash
#
# make demo-k8s -- the Kubernetes path, from nothing to a delivered webhook.
#
# Creates a single-node k3d cluster, provisions everything with Terraform
# (CloudNativePG, Valkey, ArgoCD, Sealed Secrets, ingress-nginx, Prometheus,
# Grafana, Tempo), waits for ArgoCD to sync the application, deploys a test
# receiver, delivers a signed webhook end to end, and prints where to look.
#
# Single node on purpose: pod-to-pod TCP is unreliable across k3d nodes on
# Docker Desktop for Mac (issue #21), and one node removes inter-node traffic
# from the picture entirely.
set -euo pipefail
ROOT="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"
cd "$ROOT"

NS=webhook-relay
API_PORT="${API_PORT:-18080}"
SINK_PORT="${SINK_PORT:-19090}"
CFG=deploy/k3d/cluster-single.yaml

step() { printf '\n\033[1;36m▸ %s\033[0m\n' "$*"; }
ok()   { printf '  \033[32m✓\033[0m %s\n' "$*"; }
info() { printf '    %s\n' "$*"; }
warn() { printf '  \033[33m!\033[0m %s\n' "$*"; }

for c in docker kubectl k3d terraform helm jq curl; do
  command -v "$c" >/dev/null || { echo "demo-k8s: $c is required" >&2; exit 1; }
done
docker info >/dev/null 2>&1 || { echo "demo-k8s: the Docker daemon is not running" >&2; exit 1; }

MEM=$(docker info --format '{{.MemTotal}}' 2>/dev/null || echo 0)
if [ "$MEM" -lt 4500000000 ]; then
  warn "Docker has $(( MEM / 1024 / 1024 ))MiB. This profile wants ~4.8GiB and will"
  info "likely fail during the ArgoCD install with a resource-quota timeout."
  info "Docker Desktop -> Settings -> Resources -> Memory."
fi

step "creating the cluster"
if k3d cluster list 2>/dev/null | grep -q '^webhook-relay '; then
  info "cluster exists; reusing it (make cluster-down to start clean)"
else
  k3d cluster create --config "$CFG" >/dev/null
fi
kubectl config use-context k3d-webhook-relay >/dev/null 2>&1 || true
ok "$(kubectl get nodes --no-headers | wc -l | tr -d ' ') node ready"

step "provisioning with Terraform (several minutes on a cold cache)"
make bootstrap PROFILE=lowmem 2>&1 | grep -E "Apply complete|Error:" | tail -2 || true
kubectl wait --for=condition=Available --timeout=600s \
  deploy/webhook-relay-api deploy/webhook-relay-worker -n "$NS" 2>&1 | tail -2
ok "application deployments available"

step "waiting for ArgoCD to report Synced and Healthy"
for _ in $(seq 1 60); do
  OUT=$(kubectl get applications -n argocd --no-headers 2>/dev/null || true)
  [ -n "$OUT" ] && ! grep -qvE 'Synced +Healthy' <<<"$OUT" && break
  sleep 5
done
kubectl get applications -n argocd --no-headers 2>/dev/null | awk '{printf "    %-24s %s %s\n", $1, $2, $3}'

step "deploying a test receiver"
docker build -q -t localhost:5111/webhook-sink:dev -f Dockerfile.sink . >/dev/null
docker push -q localhost:5111/webhook-sink:dev >/dev/null
kubectl apply -n "$NS" -f deploy/k8s/demo-sink.yaml >/dev/null
kubectl wait --for=condition=Ready pod -l app.kubernetes.io/name=webhook-relay-sink -n "$NS" --timeout=180s >/dev/null
ok "receiver ready"

kubectl port-forward -n "$NS" svc/webhook-relay-api "${API_PORT}:80"   >/dev/null 2>&1 &
kubectl port-forward -n "$NS" svc/webhook-relay-sink "${SINK_PORT}:9090" >/dev/null 2>&1 &
trap 'pkill -f "port-forward -n '"$NS"'" 2>/dev/null || true' EXIT
sleep 8

step "delivering a signed webhook"
info "readiness: $(curl -fsS "http://localhost:${API_PORT}/readyz")"
EP=$(curl -fsS -X POST "http://localhost:${API_PORT}/v1/endpoints" -H 'Content-Type: application/json' \
  -d "{\"url\":\"http://webhook-relay-sink.${NS}.svc.cluster.local:9090/hook\",\"description\":\"demo-k8s\",\"rate_limit_per_sec\":100}" | jq -r .id)
info "endpoint $EP"
EV=$(curl -fsS -X POST "http://localhost:${API_PORT}/v1/events" -H 'Content-Type: application/json' \
  -d "{\"endpoint_id\":\"${EP}\",\"event_type\":\"order.created\",\"payload\":{\"order\":\"K8S-DEMO\"}}" | jq -r .id)
info "event    $EV"

ST=pending
for _ in $(seq 1 40); do
  ST=$(curl -fsS "http://localhost:${API_PORT}/v1/events/${EV}" | jq -r .status)
  [ "$ST" = "delivered" ] && break
  sleep 2
done
if [ "$ST" = "delivered" ]; then
  ok "status: ${ST}"
  info "receiver recorded: $(curl -fsS "http://localhost:${SINK_PORT}/_control/stats" | jq -c '{total, distinct_events}')"
else
  warn "status: ${ST} -- see 'kubectl logs -n ${NS} -l app.kubernetes.io/component=worker'"
  exit 1
fi

step "where to look"
GRAFANA_PW=$(kubectl -n observability get secret kube-prometheus-stack-grafana -o jsonpath='{.data.admin-password}' 2>/dev/null | base64 -d 2>/dev/null || echo '(see make endpoints)')
ARGOCD_PW=$(kubectl -n argocd get secret argocd-initial-admin-secret -o jsonpath='{.data.password}' 2>/dev/null | base64 -d 2>/dev/null || echo '(see make endpoints)')
cat <<EOF

  kubectl port-forward -n observability svc/kube-prometheus-stack-grafana 3000:80
      Grafana    http://localhost:3000     admin / ${GRAFANA_PW}

  kubectl port-forward -n argocd svc/argocd-server 8080:80
      ArgoCD     http://localhost:8080     admin / ${ARGOCD_PW}

  kubectl port-forward -n chaos-testing svc/chaos-dashboard 2333:2333
      Chaos Mesh http://localhost:2333     (if installed; see make chaos-list)

  make cluster-down    destroy the cluster and its registry

EOF
ok "demo-k8s complete"
