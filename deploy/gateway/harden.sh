#!/bin/sh
# Hardens a fresh Debian/Ubuntu VPS for Rubi Gateway. Run as root (sudo sh harden.sh). Idempotent.
#
# Result: no public inbound ports at all (after CLOSE_PUBLIC_SSH=1). Rubi Gateway is published only through
# a Cloudflare Tunnel (outbound), and SSH is reachable only over Tailscale. Security updates install
# automatically.
set -eu

say() { printf 'harden: %s\n' "$*"; }

export DEBIAN_FRONTEND=noninteractive
apt-get update -qq
apt-get install -y -qq unattended-upgrades ufw curl ca-certificates gnupg >/dev/null

say "automatic security updates"
cat > /etc/apt/apt.conf.d/20auto-upgrades <<'CFG'
APT::Periodic::Update-Package-Lists "1";
APT::Periodic::Unattended-Upgrade "1";
CFG

say "Tailscale"
if ! command -v tailscale >/dev/null; then
  curl -fsSL https://tailscale.com/install.sh | sh >/dev/null
fi
systemctl enable --now tailscaled >/dev/null

say "cloudflared"
if ! command -v cloudflared >/dev/null; then
  mkdir -p --mode=0755 /usr/share/keyrings
  curl -fsSL https://pkg.cloudflare.com/cloudflare-main.gpg -o /usr/share/keyrings/cloudflare-main.gpg
  echo "deb [signed-by=/usr/share/keyrings/cloudflare-main.gpg] https://pkg.cloudflare.com/cloudflared any main" \
    > /etc/apt/sources.list.d/cloudflared.list
  apt-get update -qq && apt-get install -y -qq cloudflared >/dev/null
fi

say "SSH: keys only, no root, no passwords"
mkdir -p /etc/ssh/sshd_config.d
cat > /etc/ssh/sshd_config.d/10-rubi-hardening.conf <<'CFG'
PermitRootLogin no
PasswordAuthentication no
KbdInteractiveAuthentication no
PubkeyAuthentication yes
X11Forwarding no
AllowAgentForwarding no
AllowTcpForwarding no
MaxAuthTries 3
LoginGraceTime 20
ClientAliveInterval 300
CFG
sshd -t && systemctl reload ssh 2>/dev/null || systemctl reload sshd

say "kernel network hardening"
cat > /etc/sysctl.d/90-rubi.conf <<'CFG'
net.ipv4.conf.all.rp_filter = 1
net.ipv4.conf.all.accept_redirects = 0
net.ipv6.conf.all.accept_redirects = 0
net.ipv4.conf.all.send_redirects = 0
net.ipv4.conf.all.accept_source_route = 0
net.ipv4.icmp_echo_ignore_broadcasts = 1
net.ipv4.tcp_syncookies = 1
kernel.kptr_restrict = 2
kernel.dmesg_restrict = 1
CFG
sysctl -q --system

say "firewall: deny all inbound except SSH"
ufw --force reset >/dev/null
ufw default deny incoming >/dev/null
ufw default allow outgoing >/dev/null
ufw allow in on tailscale0 to any port 22 proto tcp >/dev/null
if [ "${CLOSE_PUBLIC_SSH:-0}" = 1 ]; then
  say "public SSH closed: SSH works only over Tailscale now"
else
  # Kept until SSH over Tailscale is confirmed, so nobody gets locked out. Then run with CLOSE_PUBLIC_SSH=1.
  ufw limit 22/tcp >/dev/null
  say "public SSH still open (rate-limited); close it with CLOSE_PUBLIC_SSH=1 once Tailscale SSH works"
fi
ufw --force enable >/dev/null

say "done"
