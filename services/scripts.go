package services

// Scripts returns the standard shell scripts for a project directory.
func Scripts() map[string]string {
	return map[string]string{
		"up.sh":    "#!/usr/bin/env bash\nset -euo pipefail\ndocker compose pull\ndocker compose up -d\ndocker compose ps\n",
		"down.sh":  "#!/usr/bin/env bash\nset -euo pipefail\ndocker compose down\n",
		"purge.sh": "#!/usr/bin/env bash\nset -euo pipefail\ndocker compose down -v --remove-orphans\n",
	}
}

// RouterScript returns the bootstrap script for a debian-router container.
func RouterScript() string {
	return `#!/usr/bin/env bash
set -e

echo '[Router] Installation des paquets reseau (iptables, iproute2)...'
export DEBIAN_FRONTEND=noninteractive
apt-get update -y && apt-get install -y iproute2 iptables curl && apt-get clean

echo '[Router] Activation IP Forwarding...'
sysctl -w net.ipv4.ip_forward=1

echo '[Router] Detection de l interface WAN...'
# On cherche l'interface qui a la route par defaut
WAN_IF=$(ip -o -4 route show default | awk '{print $5}' | head -n1)
if [ -z "$WAN_IF" ]; then
  echo '[Router] Attention: Pas de route par defaut detectee. Fallback sur eth0.'
  WAN_IF='eth0'
fi
echo "[Router] Interface WAN detectee : $WAN_IF"

echo '[Router] Application du NAT (Masquerade)...'
iptables -t nat -A POSTROUTING -o "$WAN_IF" -j MASQUERADE
iptables -P FORWARD ACCEPT

echo '[Router] Configuration terminee. Pret.'
# Garder le conteneur actif
tail -f /dev/null
`
}

// Fail2banJail returns the default jail.local content for fail2ban.
func Fail2banJail() string {
	return "[DEFAULT]\nbantime=10m\nfindtime=10m\nmaxretry=5\n[sshd]\nenabled=true\n"
}
