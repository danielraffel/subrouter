#!/usr/bin/env bash
set -euo pipefail

export DEBIAN_FRONTEND=noninteractive

apt-get update
apt-get install -y ca-certificates curl iptables

if ! command -v tailscale >/dev/null 2>&1; then
  curl -fsSL https://tailscale.com/install.sh | sh
fi

systemctl enable --now tailscaled

curl -fsSL https://github.com/manaflow-ai/subrouter/releases/latest/download/install.sh | env SUBROUTER_VERSION="${SUBROUTER_VERSION:-latest}" sh
/usr/local/bin/sr install-systemd --addr 0.0.0.0:31415 --cx-switch-interval 10m

cat >/usr/local/sbin/subrouter-tailnet-egress-block <<'SCRIPT'
#!/bin/sh
set -eu
iptables -C OUTPUT -o tailscale0 -m conntrack --ctstate ESTABLISHED,RELATED -j ACCEPT 2>/dev/null || iptables -I OUTPUT 1 -o tailscale0 -m conntrack --ctstate ESTABLISHED,RELATED -j ACCEPT
iptables -C OUTPUT -o tailscale0 -d 100.64.0.0/10 -m conntrack --ctstate NEW -j REJECT 2>/dev/null || iptables -A OUTPUT -o tailscale0 -d 100.64.0.0/10 -m conntrack --ctstate NEW -j REJECT
if command -v ip6tables >/dev/null 2>&1; then
  ip6tables -C OUTPUT -o tailscale0 -m conntrack --ctstate ESTABLISHED,RELATED -j ACCEPT 2>/dev/null || ip6tables -I OUTPUT 1 -o tailscale0 -m conntrack --ctstate ESTABLISHED,RELATED -j ACCEPT
  ip6tables -C OUTPUT -o tailscale0 -d fd7a:115c:a1e0::/48 -m conntrack --ctstate NEW -j REJECT 2>/dev/null || ip6tables -A OUTPUT -o tailscale0 -d fd7a:115c:a1e0::/48 -m conntrack --ctstate NEW -j REJECT
fi
SCRIPT
chmod 0755 /usr/local/sbin/subrouter-tailnet-egress-block

cat >/etc/systemd/system/subrouter-tailnet-egress-block.service <<'UNIT'
[Unit]
Description=Block new Subrouter outbound connections to tailnet devices
After=tailscaled.service
Wants=tailscaled.service

[Service]
Type=oneshot
ExecStart=/usr/local/sbin/subrouter-tailnet-egress-block
RemainAfterExit=yes

[Install]
WantedBy=multi-user.target
UNIT

systemctl enable --now subrouter-tailnet-egress-block.service

# Install the Claude rate-limit reroute self-verifier so it survives VM rebuilds.
# Best-effort: a fetch failure must never abort provisioning of the proxy itself.
install_subrouter_verify() {
  local base="https://raw.githubusercontent.com/manaflow-ai/subrouter/main/deploy/gcp"
  curl -fsSL "${base}/subrouter-verify.sh" -o /usr/local/bin/subrouter-verify.sh || return 1
  chmod 0755 /usr/local/bin/subrouter-verify.sh
  curl -fsSL "${base}/subrouter-verify.service" -o /etc/systemd/system/subrouter-verify.service || return 1
  curl -fsSL "${base}/subrouter-verify.timer" -o /etc/systemd/system/subrouter-verify.timer || return 1
  systemctl daemon-reload
  systemctl enable --now subrouter-verify.timer
}
install_subrouter_verify || echo "startup: subrouter-verify install failed (non-fatal)"
