#!/usr/bin/env bash
set -euo pipefail

export DEBIAN_FRONTEND=noninteractive

apt-get update
apt-get install -y ca-certificates curl iptables

if ! command -v tailscale >/dev/null 2>&1; then
  curl -fsSL https://tailscale.com/install.sh | sh
fi

systemctl enable --now tailscaled

curl -fsSL https://raw.githubusercontent.com/manaflow-ai/subrouter/main/install.sh | env SUBROUTER_VERSION="${SUBROUTER_VERSION:-latest}" sh
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
