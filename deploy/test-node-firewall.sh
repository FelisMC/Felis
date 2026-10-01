#!/bin/bash
# Linux/root acceptance of resident-node and pre-DNAT paths. All packet rules,
# listeners and links live in disposable network namespaces, never the live host.
set -euo pipefail
bin="${1:?usage: test-node-firewall.sh /absolute/path/to/felis}"
[[ $bin = /* && -x $bin ]] || exit 2
[[ $(id -u) = 0 ]] || { echo 'requires root on Linux' >&2; exit 2; }
work=$(mktemp -d)
suffix="$$"
node="felis-fw-node-$suffix"
game="felis-fw-game-$suffix"
peer="felis-fw-peer-$suffix"
listener=''
cleanup() {
  if [[ -n $listener ]]; then kill "$listener" 2>/dev/null || true; wait "$listener" 2>/dev/null || true; fi
  for ns in "$game" "$peer" "$node"; do ip netns del "$ns" 2>/dev/null || true; done
  rm -rf "$work"
}
trap cleanup EXIT
for ns in "$node" "$game" "$peer"; do ip netns add "$ns"; ip -n "$ns" link set lo up; done
ip link add fg-node type veth peer name fg-game
ip link set fg-node netns "$node"
ip link set fg-game netns "$game"
ip link add fp-node type veth peer name fp-peer
ip link set fp-node netns "$node"
ip link set fp-peer netns "$peer"
ip -n "$node" addr add 10.42.250.1/24 dev fg-node
ip -n "$game" addr add 10.42.250.2/24 dev fg-game
ip -n "$node" addr add 192.0.2.2/24 dev fp-node
ip -n "$peer" addr add 192.0.2.1/24 dev fp-peer
ip -n "$node" link set fg-node up
ip -n "$node" link set fp-node up
ip -n "$game" link set fg-game up
ip -n "$peer" link set fp-peer up
ip -n "$game" route add 192.0.2.0/24 via 10.42.250.1
ip -n "$game" route add 10.43.0.1/32 via 10.42.250.1
ip netns exec "$node" "$bin" node-probe --listen :18083 >"$work/listener.log" 2>&1 &
listener=$!
ip netns exec "$game" "$bin" node-probe --open 192.0.2.2:18083
ip netns exec "$peer" "$bin" node-probe --open 192.0.2.2:18083

"$bin" node firewall --dry-run --peers 192.0.2.1/32,192.0.2.2/32 \
  --controller-ip 192.0.2.1 --pod-cidr 10.42.0.0/16 \
  --node-port 30443 --api-service-ip 10.43.0.1 >"$work/firewall.sh"
ip netns exec "$node" bash "$work/firewall.sh"
# Simulate later kube-router insertion ahead of Felis filter hooks.
ip netns exec "$node" iptables -I INPUT 1 -j ACCEPT
ip netns exec "$node" iptables -t nat -A PREROUTING -p tcp --dport 30443 -j REDIRECT --to-ports 18083
ip netns exec "$node" iptables -t nat -A PREROUTING -d 10.43.0.1 -p tcp --dport 443 -j REDIRECT --to-ports 18083
ip netns exec "$game" "$bin" node-probe \
  --closed 192.0.2.2:18083 --closed 192.0.2.2:30443 --closed 10.43.0.1:443
ip netns exec "$peer" "$bin" node-probe --closed 192.0.2.2:30443
# The listener remains healthy and the allowed peer still reaches it.
ip netns exec "$peer" "$bin" node-probe --open 192.0.2.2:18083
ip netns exec "$node" "$bin" node-probe --open 127.0.0.1:18083
echo 'PASS resident-node isolation and public NodePort denial survive pre-DNAT and early filter ACCEPT'
