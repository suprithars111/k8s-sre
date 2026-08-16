# Demo & verification guide

A guided tour for showing the tool working end to end against a local kind
cluster, with the expected output at each step. For the automated version of
this entire flow, run `scripts/demo-e2e.sh` - 28 explicit PASS/FAIL
assertions, exit code tells you the answer.

## Setup (~5 min)

Cluster creation + Calico is the slow part:

```bash
kind create cluster --name sre-demo --config deploy/kind-config.yaml
kubectl apply -f https://raw.githubusercontent.com/projectcalico/calico/v3.28.2/manifests/calico.yaml
kubectl -n kube-system rollout status daemonset/calico-node --timeout=240s
```

Sanity-check everything once, then reset the demo namespaces:

```bash
./scripts/demo-e2e.sh          # expect: Results: 28 passed, 0 failed
./scripts/demo-e2e.sh --cleanup
```

Worth knowing: kind's default CNI (kindnet) does not enforce NetworkPolicy -
policies would be created but traffic would still flow. That's why the demo
uses Calico, and it's a real operational gotcha on any cluster.

## Tour (~10 min)

### 0. Build, deploy, expose

```bash
docker build -t k8s-sre-tool:dev .
kind load docker-image k8s-sre-tool:dev --name sre-demo

helm upgrade --install k8s-sre-tool helm/k8s-sre-tool \
  --namespace sre-tool --create-namespace \
  --set image.repository=k8s-sre-tool --set image.tag=dev
kubectl -n sre-tool rollout status deployment/k8s-sre-tool

kubectl -n sre-tool port-forward svc/k8s-sre-tool 8080:8080 &
```

The CI half of the image build lives on GitHub: the **Actions** tab shows the
test + lint + build pipeline, and **Packages** holds the pushed multi-arch
image (`latest` + `sha-<commit>`).

### 1. Connectivity (positive)

```bash
curl -s http://localhost:8080/readyz
```

Expected: `{"apiServerReachable":true,"serverVersion":"v1.36.1"}`

This endpoint *is* the readiness probe, so `kubectl get pods -n sre-tool`
shows `Ready` only while the API server is reachable - a platform-native,
alertable signal, no polling required.

### 2. Deployment health

```bash
kubectl apply -f deploy/demo.yaml
kubectl -n team-a rollout status deployment/web
curl -s http://localhost:8080/api/v1/deployments/health | jq
```

Expected: `"healthy": false`, `"unhealthyCount": 1`, and `team-b/broken`
showing `"reason": "0/2 replicas ready"` - its image tag intentionally doesn't
exist. Everything else healthy. Scope it:

```bash
curl -s 'http://localhost:8080/api/v1/deployments/health?namespace=team-a' | jq
```

### 3. Isolation

Always show the "before" first:

```bash
kubectl -n team-b exec deploy/client -- wget -qO- --timeout=3 http://web.team-a.svc | head -4
```

Expected: nginx welcome page. Now isolate:

```bash
curl -s -X POST http://localhost:8080/api/v1/isolations \
  -H 'Content-Type: application/json' \
  -d '{"workloadA": {"namespace": "team-a", "labelSelector": "app=web"},
       "workloadB": {"namespace": "team-b", "labelSelector": "app=client"}}' | jq
```

Expected: `"status": "isolated"` + two policies. Prove all three properties:

```bash
# 1. blocked: client -> web
kubectl -n team-b exec deploy/client -- wget -qO- --timeout=3 http://web.team-a.svc || echo BLOCKED

# 2. blocked in reverse (ICMP shows it's L3, not just HTTP)
CLIENT_IP=$(kubectl -n team-b get pod -l app=client -o jsonpath='{.items[0].status.podIP}')
kubectl -n team-a exec deploy/web -- ping -c 2 -W 2 $CLIENT_IP || echo BLOCKED

# 3. surgical: a pod WITHOUT the app=client label still gets through
kubectl -n team-b run bystander --image=busybox:1.36 --restart=Never -i --rm --quiet \
  --command -- wget -qO- --timeout=8 http://web.team-a.svc | head -4
```

Show what was actually created - the policy YAML is where the design lives
(allow-list semantics, per-label `NotIn` negation, ingress on both sides):

```bash
curl -s http://localhost:8080/api/v1/isolations | jq
kubectl -n team-a get networkpolicy -o yaml
```

Restore and verify:

```bash
curl -s -X DELETE http://localhost:8080/api/v1/isolations \
  -H 'Content-Type: application/json' \
  -d '{"workloadA": {"namespace": "team-a", "labelSelector": "app=web"},
       "workloadB": {"namespace": "team-b", "labelSelector": "app=client"}}' | jq
kubectl -n team-b exec deploy/client -- wget -qO- --timeout=3 http://web.team-a.svc | head -4
kubectl get networkpolicy -A   # expect: No resources found
```

### 4. Connectivity (negative) - the outage drill

Break the tool's own egress to the API server and let Kubernetes tell the story:

```bash
kubectl -n sre-tool apply -f - <<'EOF'
apiVersion: networking.k8s.io/v1
kind: NetworkPolicy
metadata:
  name: cut-api-access
  namespace: sre-tool
spec:
  podSelector: {}
  policyTypes: [Egress]
EOF
kubectl -n sre-tool rollout restart deployment/k8s-sre-tool
```

Wait ~30-45s, then:

```bash
kubectl -n sre-tool get pods
kubectl -n sre-tool get events --field-selector reason=Unhealthy | tail -2
```

Expected: new pod `0/1 Running`, event `Readiness probe failed: HTTP probe
failed with statuscode: 503` - while the old replica stays Ready and serving
(the rolling update protecting availability).

Why the restart? client-go holds a persistent connection and CNIs don't cut
established conntrack flows, so a partition blocking only *new* connections
isn't seen until the connection breaks. The restart forces a fresh one. This
is an inherent property of any live-check design, documented in the README.

Recover:

```bash
kubectl -n sre-tool delete networkpolicy cut-api-access
kubectl -n sre-tool rollout status deployment/k8s-sre-tool
```

## How correctness is verified, in layers

1. **Unit/integration tests** - `go test -race -cover ./...`: fake clientset
   for the Kubernetes logic (including failure paths via reactors),
   `httptest` over the real router for the HTTP layer.
2. **CI** - every PR and push runs gofmt, vet, race-enabled tests, helm lint +
   template render; `main` additionally builds and pushes the multi-arch image.
3. **End-to-end** - `scripts/demo-e2e.sh`: 28 assertions against a real kind
   cluster with Calico, covering every feature including the negative paths
   (blocked traffic, surgical scope, API outage → NotReady, recovery).
4. **Observable effects** - nothing above asserts "trust me": every claim is
   checked via an independent witness (wget/ping from inside pods, kubelet
   probe events, pod Ready conditions, empty NetworkPolicy listings).

## If the environment misbehaves

- Reset demo state: `./scripts/demo-e2e.sh --cleanup`, then re-apply
  `deploy/demo.yaml`.
- Nuke and rebuild (~4 min): `kind delete cluster --name sre-demo`, then Setup.
- Port-forward died (it does after pod restarts): `pkill -f port-forward`,
  re-run the port-forward command.
