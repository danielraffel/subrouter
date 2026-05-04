#!/usr/bin/env bash
set -euo pipefail

export DEBIAN_FRONTEND=noninteractive

apt-get update
apt-get install -y ca-certificates curl iptables

if ! command -v tailscale >/dev/null 2>&1; then
  curl -fsSL https://tailscale.com/install.sh | sh
fi

systemctl enable --now tailscaled

if ! id -u subrouter >/dev/null 2>&1; then
  useradd --system --home-dir /var/lib/subrouter --create-home --shell /usr/sbin/nologin subrouter
fi

install -d -o subrouter -g subrouter -m 0750 /var/lib/subrouter
install -d -o subrouter -g subrouter -m 0750 /var/lib/subrouter/transcripts
install -d -o subrouter -g subrouter -m 0750 /var/lib/subrouter/.codex
install -d -o subrouter -g subrouter -m 0750 /var/lib/subrouter/.codex-accounts/accounts
install -d -o subrouter -g subrouter -m 0750 /var/log/subrouter

subrouter_extra_args=""
if [[ -f /etc/default/subrouter ]]; then
  # Preserve optional deployment-specific flags such as GCS mirroring.
  # shellcheck disable=SC1091
  . /etc/default/subrouter
  subrouter_extra_args="${SUBROUTER_EXTRA_ARGS:-}"
fi

cat >/etc/default/subrouter <<CONFIG
SUBROUTER_ADDR=0.0.0.0:31415
SUBROUTER_SESSIONS=/var/lib/subrouter/sessions.json
SUBROUTER_TRANSCRIPTS=/var/lib/subrouter/transcripts
SUBROUTER_CX_SWITCH_INTERVAL=10m
SUBROUTER_EXTRA_ARGS="${subrouter_extra_args}"
CONFIG

cat >/etc/systemd/system/subrouter.service <<'UNIT'
[Unit]
Description=Subrouter AI agent router
Wants=network-online.target tailscaled.service
After=network-online.target tailscaled.service

[Service]
Type=simple
User=subrouter
Group=subrouter
WorkingDirectory=/var/lib/subrouter
Environment=HOME=/var/lib/subrouter
EnvironmentFile=-/etc/default/subrouter
ExecStart=/usr/local/bin/subrouter serve --addr ${SUBROUTER_ADDR} --sessions ${SUBROUTER_SESSIONS} --transcripts ${SUBROUTER_TRANSCRIPTS} --cx-switch-interval ${SUBROUTER_CX_SWITCH_INTERVAL} $SUBROUTER_EXTRA_ARGS
Restart=on-failure
RestartSec=3
NoNewPrivileges=true
PrivateTmp=true
ProtectSystem=full
ProtectHome=true
ReadWritePaths=/var/lib/subrouter /var/log/subrouter

[Install]
WantedBy=multi-user.target
UNIT

systemctl daemon-reload
systemctl enable subrouter

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

systemctl daemon-reload
systemctl enable --now subrouter-tailnet-egress-block.service

if [[ -x /usr/local/bin/subrouter ]]; then
  ln -sf /usr/local/bin/subrouter /usr/local/bin/cx
  systemctl restart subrouter
fi
