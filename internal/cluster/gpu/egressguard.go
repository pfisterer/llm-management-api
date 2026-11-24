package gpu

import (
	"net"
	"strings"
)

// egressGuardScript waits until the pod's egress NetworkPolicy is in force before anything else in the pod runs. kube-router installs a new pod's chains only up to ~5 s after the pod got its address; until then the pod reaches everything (measured 17.10.2026: a build pod reached the main node's SSH and the DHBW's internal hosts at 0 s, not at 5 s). The guard connects to an address the policy must block (the Kubernetes API's service address, which is rewritten to the main node before the policy looks) and exits 0 once two attempts in a row fail; is the address still reachable after 30 s, the pod fails instead of starting open. Bash's /dev/tcp and coreutils' timeout only, so it runs in the hub's init image and in the builder image alike.
const egressGuardScript = `H="$1"; P="$2"; n=0; end=$((SECONDS+30))
while [ $SECONDS -lt $end ]; do
  if timeout 1 bash -c "exec 3<>/dev/tcp/$H/$P" 2>/dev/null; then n=0; sleep 0.3
  else n=$((n+1)); if [ $n -ge 2 ]; then echo "dhbw: egress policy in force after ${SECONDS} s"; exit 0; fi; fi
done
M="Die Netzwerkregeln dieses Pods greifen nach 30 s noch nicht (${H}:${P} erreichbar); der Start wird abgebrochen. Bitte erneut versuchen."
echo "dhbw: $M" >&2; echo "$M" > /dev/termination-log; exit 1`

// egressGuardInit is the init container that runs first in pods with people's code (builds, batch jobs, GPU kernels); target is host:port of an address the pod's egress policy blocks. Nil when target is empty or no image is given: the pod then starts as before.
func egressGuardInit(image, target string, sec map[string]any) map[string]any {
	host, port, err := net.SplitHostPort(strings.TrimSpace(target))
	if image == "" || err != nil || host == "" || port == "" {
		return nil
	}
	return map[string]any{
		"name":            "dhbw-egress-guard",
		"image":           image,
		"command":         []any{"bash", "-c", egressGuardScript, "dhbw-egress-guard", host, port},
		"securityContext": sec,
		"resources":       map[string]any{"requests": map[string]any{"cpu": "10m", "memory": "16Mi"}, "limits": map[string]any{"memory": "64Mi"}},
	}
}

// withEgressGuard puts the guard in front of the pod spec's init containers (no-op without a guard).
func withEgressGuard(spec map[string]any, guard map[string]any) {
	if guard == nil {
		return
	}
	inits, _ := spec["initContainers"].([]any)
	spec["initContainers"] = append([]any{guard}, inits...)
}
