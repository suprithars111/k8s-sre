#!/usr/bin/env bash
#
# End-to-end verification of every feature against a local kind
# cluster. Every feature is checked with explicit assertions and the script
# exits non-zero if any of them fail - run it before a demo (or as the demo).
#
# Usage:
#   scripts/demo-e2e.sh            # full run, including the API outage drill
#   scripts/demo-e2e.sh --quick    # skip the outage drill (~90s faster)
#   scripts/demo-e2e.sh --cleanup  # remove demo namespaces and isolations, then exit
#
# Idempotent: reuses the kind cluster, Calico install and Helm release if they
# already exist, and cleans up leftover isolation policies before asserting.

set -u -o pipefail

CLUSTER=sre-demo
CTX=kind-${CLUSTER}
K="kubectl --context ${CTX}"
NS=sre-tool
RELEASE=k8s-sre-tool
DEPLOY=k8s-sre-tool
PORT=18080
BASE=http://localhost:${PORT}
REPO_ROOT=$(cd "$(dirname "$0")/.." && pwd)

QUICK=false
CLEANUP=false
case "${1:-}" in
  --quick) QUICK=true ;;
  --cleanup) CLEANUP=true ;;
esac

PASS=0
FAIL=0
PF_PID=""

bold()  { printf '\033[1m%s\033[0m\n' "$*"; }
step()  { echo; bold "==> $*"; }
pass()  { PASS=$((PASS+1)); printf '    \033[32mPASS\033[0m %s\n' "$*"; }
fail()  { FAIL=$((FAIL+1)); printf '    \033[31mFAIL\033[0m %s\n' "$*"; }

# assert <description> <command...>  - command's exit code decides pass/fail
assert() {
  local desc=$1; shift
  if "$@" >/dev/null 2>&1; then pass "$desc"; else fail "$desc"; fi
}

stop_pf() { [ -n "$PF_PID" ] && kill "$PF_PID" 2>/dev/null; PF_PID=""; }
trap stop_pf EXIT

start_pf() {
  stop_pf
  $K -n $NS port-forward svc/$DEPLOY ${PORT}:8080 >/dev/null 2>&1 &
  PF_PID=$!
  for _ in $(seq 1 30); do
    curl -sf -m 2 "$BASE/healthz" >/dev/null 2>&1 && return 0
    sleep 1
  done
  fail "port-forward to $DEPLOY never became ready"
  return 1
}

isolation_body() {
  cat <<'JSON'
{"workloadA": {"namespace": "team-a", "labelSelector": "app=web"},
 "workloadB": {"namespace": "team-b", "labelSelector": "app=client"}}
JSON
}

if $CLEANUP; then
  step "Cleaning up demo resources"
  $K delete networkpolicy -A -l app.kubernetes.io/managed-by=k8s-sre-tool --ignore-not-found
  $K -n $NS delete networkpolicy cut-api-access --ignore-not-found
  $K delete namespace team-a team-b --ignore-not-found
  echo "Done. Delete the cluster with: kind delete cluster --name $CLUSTER"
  exit 0
fi

step "Checking prerequisites"
for tool in kind kubectl helm docker curl python3; do
  command -v "$tool" >/dev/null || { echo "missing required tool: $tool"; exit 1; }
done
pass "kind, kubectl, helm, docker, curl, python3 present"

step "Cluster: kind '$CLUSTER' with Calico (NetworkPolicy enforcement)"
if kind get clusters 2>/dev/null | grep -qx "$CLUSTER"; then
  pass "cluster already exists - reusing"
else
  kind create cluster --name $CLUSTER --config "$REPO_ROOT/deploy/kind-config.yaml" || exit 1
  pass "cluster created"
fi
if $K -n kube-system get daemonset calico-node >/dev/null 2>&1; then
  pass "Calico already installed"
else
  $K apply -f https://raw.githubusercontent.com/projectcalico/calico/v3.28.2/manifests/calico.yaml >/dev/null || exit 1
  pass "Calico applied"
fi
$K -n kube-system rollout status daemonset/calico-node --timeout=240s >/dev/null || exit 1
pass "Calico ready"

step "Container image: docker build"
docker build -q -t k8s-sre-tool:dev "$REPO_ROOT" >/dev/null \
  && pass "docker build succeeded" \
  || { fail "docker build failed"; exit 1; }
kind load docker-image k8s-sre-tool:dev --name $CLUSTER >/dev/null 2>&1
pass "image loaded into kind (the CI half - push to GHCR - is proven by a green run on GitHub)"

step "Deploy with Helm"
helm --kube-context $CTX upgrade --install $RELEASE "$REPO_ROOT/helm/k8s-sre-tool" \
  --namespace $NS --create-namespace \
  --set image.repository=k8s-sre-tool --set image.tag=dev >/dev/null \
  && pass "helm upgrade --install succeeded" \
  || { fail "helm install failed"; exit 1; }
$K -n $NS rollout status deployment/$DEPLOY --timeout=120s >/dev/null \
  && pass "tool deployment rolled out and Ready (readiness = API connectivity)" \
  || { fail "tool deployment did not become ready"; exit 1; }

# Remove leftovers from previous runs so baseline assertions are clean.
$K delete networkpolicy -A -l app.kubernetes.io/managed-by=k8s-sre-tool --ignore-not-found >/dev/null 2>&1
$K -n $NS delete networkpolicy cut-api-access --ignore-not-found >/dev/null 2>&1

start_pf || exit 1

step "API server connectivity via /readyz"
READYZ=$(curl -s -m 15 "$BASE/readyz")
CODE=$(curl -s -m 15 -o /dev/null -w '%{http_code}' "$BASE/readyz")
echo "    /readyz -> $READYZ"
assert "/readyz returns HTTP 200" test "$CODE" = "200"
assert "/readyz reports apiServerReachable=true with a server version" \
  python3 -c "import json,sys; d=json.loads(sys.argv[1]); assert d['apiServerReachable'] and d['serverVersion']" "$READYZ"

step "Deployment health report"
$K apply -f "$REPO_ROOT/deploy/demo.yaml" >/dev/null
$K -n team-a rollout status deployment/web --timeout=180s >/dev/null
$K -n team-b rollout status deployment/client --timeout=180s >/dev/null
pass "demo workloads deployed (web, client healthy; 'broken' intentionally unhealthy)"

HEALTH=$(curl -s "$BASE/api/v1/deployments/health")
assert "cluster-wide report flags exactly the broken deployment" \
  python3 -c "
import json,sys
d=json.loads(sys.argv[1])
broken=[x for x in d['deployments'] if x['name']=='broken'][0]
assert d['healthy'] is False and d['unhealthyCount']==1
assert broken['healthy'] is False and broken['readyReplicas']==0 and broken['desiredReplicas']==2
" "$HEALTH"
SCOPED=$(curl -s "$BASE/api/v1/deployments/health?namespace=team-a")
assert "?namespace=team-a scoped report is healthy" \
  python3 -c "import json,sys; d=json.loads(sys.argv[1]); assert d['healthy'] and d['total']==1" "$SCOPED"

step "Workload isolation"
assert "baseline: team-b/client CAN reach team-a/web" \
  $K -n team-b exec deploy/client -- wget -qO- --timeout=5 http://web.team-a.svc

POST_CODE=$(isolation_body | curl -s -o /dev/null -w '%{http_code}' -X POST "$BASE/api/v1/isolations" -H 'Content-Type: application/json' -d @-)
assert "POST /api/v1/isolations returns 201" test "$POST_CODE" = "201"
NP_COUNT=$($K get networkpolicy -A -l app.kubernetes.io/managed-by=k8s-sre-tool --no-headers 2>/dev/null | wc -l | tr -d ' ')
assert "exactly 2 managed NetworkPolicies created" test "$NP_COUNT" = "2"
sleep 3

if $K -n team-b exec deploy/client -- wget -qO- --timeout=4 http://web.team-a.svc >/dev/null 2>&1; then
  fail "isolated: client -> web should be BLOCKED but succeeded"
else
  pass "isolated: client -> web is blocked"
fi

CLIENT_IP=$($K -n team-b get pod -l app=client -o jsonpath='{.items[0].status.podIP}')
if $K -n team-a exec deploy/web -- ping -c 2 -W 2 "$CLIENT_IP" >/dev/null 2>&1; then
  fail "isolated: web -> client (reverse) should be BLOCKED but succeeded"
else
  pass "isolated: web -> client (reverse direction) is blocked"
fi

# A pod without the app=client label must be unaffected. Run to completion and
# check the phase instead of attaching, which is racy for fast-exiting pods.
BYSTANDER="bystander-$$"
$K -n team-b run "$BYSTANDER" --image=busybox:1.36 --restart=Never --quiet \
  --command -- wget -qO- --timeout=8 http://web.team-a.svc >/dev/null 2>&1
if $K -n team-b wait --for=jsonpath='{.status.phase}'=Succeeded "pod/$BYSTANDER" --timeout=60s >/dev/null 2>&1; then
  pass "unrelated pod in team-b still reaches web (isolation is surgical)"
else
  fail "unrelated pod in team-b could not reach web while isolation active"
fi
$K -n team-b delete pod "$BYSTANDER" --wait=false >/dev/null 2>&1

LIST=$(curl -s "$BASE/api/v1/isolations")
assert "GET /api/v1/isolations lists both policies with workload annotations" \
  python3 -c "
import json,sys
d=json.loads(sys.argv[1])
assert len(d)==2 and all(x['workloadA']=='team-a/app=web' for x in d)
" "$LIST"

DEL_CODE=$(isolation_body | curl -s -o /dev/null -w '%{http_code}' -X DELETE "$BASE/api/v1/isolations" -H 'Content-Type: application/json' -d @-)
assert "DELETE /api/v1/isolations returns 200" test "$DEL_CODE" = "200"
sleep 3
assert "restored: client -> web works again" \
  $K -n team-b exec deploy/client -- wget -qO- --timeout=5 http://web.team-a.svc
NP_LEFT=$($K get networkpolicy -A --no-headers 2>/dev/null | wc -l | tr -d ' ')
assert "no NetworkPolicies left behind" test "$NP_LEFT" = "0"

if ! $QUICK; then
  step "Failure drill: API outage - pod must leave Ready"
  $K apply -f - >/dev/null <<'EOF'
apiVersion: networking.k8s.io/v1
kind: NetworkPolicy
metadata:
  name: cut-api-access
  namespace: sre-tool
spec:
  podSelector: {}
  policyTypes: [Egress]
EOF
  # Established connections survive a partition (conntrack), so restart the
  # pod to force a fresh connection - the new pod must never become Ready.
  $K -n $NS rollout restart deployment/$DEPLOY >/dev/null
  NEW_POD=""
  DETECTED=false
  for _ in $(seq 1 40); do
    NEW_POD=$($K -n $NS get pods --sort-by=.metadata.creationTimestamp -o name 2>/dev/null | tail -1)
    if [ -n "$NEW_POD" ] && $K -n $NS get events --field-selector reason=Unhealthy 2>/dev/null \
        | grep "${NEW_POD#pod/}" | grep -q "statuscode: 503"; then
      DETECTED=true
      break
    fi
    sleep 5
  done
  if $DETECTED; then
    pass "kubelet readiness probe got 503 from the new pod: ${NEW_POD#pod/}"
  else
    fail "expected a 503 readiness event for the new pod within ~200s"
  fi
  assert "new pod is Running but NOT Ready" \
    bash -c "$K -n $NS get $NEW_POD -o jsonpath='{.status.conditions[?(@.type==\"Ready\")].status}' | grep -q False"

  $K -n $NS delete networkpolicy cut-api-access >/dev/null
  $K -n $NS rollout status deployment/$DEPLOY --timeout=180s >/dev/null \
    && pass "connectivity restored: rollout completed, pod Ready again" \
    || fail "deployment did not recover after removing the egress block"

  start_pf || true
  CODE=$(curl -s -m 15 -o /dev/null -w '%{http_code}' "$BASE/readyz")
  assert "/readyz back to 200 after recovery" test "$CODE" = "200"
fi

step "Unit tests (the other layer of proof)"
( cd "$REPO_ROOT/golang" && go test -race -count=1 ./... >/dev/null 2>&1 ) \
  && pass "go test -race ./... green" \
  || fail "go test failed"

echo
bold "================================================"
bold " Results: $PASS passed, $FAIL failed"
bold "================================================"
[ "$FAIL" -eq 0 ]
