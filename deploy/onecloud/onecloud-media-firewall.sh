#!/bin/sh
set -eu

IPTABLES=/usr/sbin/iptables
CHAIN=ONECLOUD_MEDIA_LAN
HOST_IP=192.168.137.11
LAN_CIDR=192.168.137.0/24
PORTS=13325,13326

"$IPTABLES" -w -nL "$CHAIN" >/dev/null 2>&1 || "$IPTABLES" -w -N "$CHAIN"
"$IPTABLES" -w -F "$CHAIN"
"$IPTABLES" -w -A "$CHAIN" -d "$HOST_IP" -s "$LAN_CIDR" -p tcp -m multiport --dports "$PORTS" -j ACCEPT
"$IPTABLES" -w -A "$CHAIN" -d "$HOST_IP" -p tcp -m multiport --dports "$PORTS" -j DROP
"$IPTABLES" -w -A "$CHAIN" -j RETURN

if ! "$IPTABLES" -w -C INPUT -j "$CHAIN" >/dev/null 2>&1; then
	"$IPTABLES" -w -I INPUT 1 -j "$CHAIN"
fi
