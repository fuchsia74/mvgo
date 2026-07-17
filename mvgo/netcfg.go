package main

import "fmt"

// buildNetcfgScript 生成在 guest 内配置静态 IP 的 bash 脚本,自动识别网络管理方式。
// 检测顺序:netplan -> nmcli -> systemd-networkd -> ifupdown。
// 与 Python 版 build_netcfg_script 行为逐字一致:Python 侧只算好
// IP/前缀/掩码/网关/DNS/网卡传下去,检测与差异化落在 guest 端脚本。
func buildNetcfgScript(ipCIDR, gateway, dns, nmcliCon, netDevice string) (string, error) {
	ip, prefix, err := splitCIDR(ipCIDR)
	if err != nil {
		return "", err
	}
	netmask, err := prefixToNetmask(prefix)
	if err != nil {
		return "", err
	}
	dnsVal := spaceJoinDNS(dns)

	// 变量头:值经 shellQuote 转义后作为 bash 变量注入
	header := "set -x\n" +
		fmt.Sprintf("IP=%s\n", shellQuote(ip)) +
		fmt.Sprintf("PREFIX=%s\n", shellQuote(fmt.Sprintf("%d", prefix))) +
		fmt.Sprintf("NETMASK=%s\n", shellQuote(netmask)) +
		fmt.Sprintf("GW=%s\n", shellQuote(gateway)) +
		fmt.Sprintf("DNS=%s\n", shellQuote(dnsVal)) +
		fmt.Sprintf("FORCE_DEV=%s\n", shellQuote(netDevice)) +
		fmt.Sprintf("NMCLI_CON=%s\n", shellQuote(nmcliCon))

	return header + netcfgBody, nil
}

// prefixToNetmask 把前缀长度转点分十进制掩码(如 24 -> 255.255.255.0)。
func prefixToNetmask(prefix int) (string, error) {
	if prefix < 0 || prefix > 32 {
		return "", fmt.Errorf("前缀超范围: %d", prefix)
	}
	var mask uint32
	if prefix > 0 {
		mask = ^uint32(0) << (32 - prefix)
	}
	return fmt.Sprintf("%d.%d.%d.%d",
		byte(mask>>24), byte(mask>>16), byte(mask>>8), byte(mask)), nil
}

// spaceJoinDNS 把逗号分隔的 DNS 转成空格分隔(去首尾空白)。
func spaceJoinDNS(dns string) string {
	out := ""
	for _, part := range splitAny(dns, ", ") {
		if part == "" {
			continue
		}
		if out != "" {
			out += " "
		}
		out += part
	}
	return out
}

// splitAny 按任一分隔字符切分(逗号或空格),丢弃空段。
func splitAny(s, seps string) []string {
	var out []string
	cur := ""
	for _, c := range s {
		if indexOf(seps, string(c)) >= 0 {
			if cur != "" {
				out = append(out, cur)
				cur = ""
			}
		} else {
			cur += string(c)
		}
	}
	if cur != "" {
		out = append(out, cur)
	}
	return out
}

// netcfgBody 是 guest 端配网脚本主体,与 Python build_netcfg_script 的 body 逐字一致。
const netcfgBody = `CIDR="$IP/$PREFIX"

# --- 解析目标网卡:优先 --net-device;否则默认路由网卡;再否则第一块物理以太网 ---
if [ -n "$FORCE_DEV" ]; then
    DEV="$FORCE_DEV"
else
    DEV="$(ip route show default 2>/dev/null | awk '{for(i=1;i<=NF;i++) if($i=="dev"){print $(i+1); exit}}')"
    if [ -z "$DEV" ]; then
        for p in /sys/class/net/*; do
            n="$(basename "$p")"
            [ "$n" = "lo" ] && continue
            [ "$(cat "$p/type" 2>/dev/null)" = "1" ] || continue
            case "$n" in docker*|veth*|br-*|virbr*|vnet*|tap*) continue;; esac
            DEV="$n"; break
        done
    fi
fi
if [ -z "$DEV" ]; then echo "ERROR: 找不到可配置的以太网设备"; exit 1; fi
echo "目标设备: $DEV"

apply_nmcli() {
    if [ -n "$NMCLI_CON" ]; then
        CON="$NMCLI_CON"
    else
        CON="$(nmcli -t -f GENERAL.CONNECTION device show "$DEV" 2>/dev/null | cut -d: -f2)"
    fi
    if [ -z "$CON" ] || [ "$CON" = "--" ]; then
        CON="static-$DEV"
        nmcli connection add type ethernet ifname "$DEV" con-name "$CON" 2>/dev/null || true
    fi
    echo "nmcli 连接: $CON"
    nmcli connection modify "$CON" ipv4.addresses "$CIDR" ipv4.gateway "$GW" ipv4.dns "$DNS" ipv4.method manual
    nmcli device reapply "$DEV" 2>/dev/null || { nmcli connection down "$CON" 2>/dev/null || true; nmcli connection up "$CON"; }
}

apply_netplan() {
    DNSC="$(echo "$DNS" | tr ' ' ',')"
    F=/etc/netplan/99-clone-static.yaml
    cat > "$F" <<EOF
network:
  version: 2
  ethernets:
    $DEV:
      dhcp4: false
      addresses: [$CIDR]
      routes:
        - to: 0.0.0.0/0
          via: $GW
      nameservers:
        addresses: [$DNSC]
EOF
    chmod 600 "$F"
    netplan apply
}

apply_networkd() {
    F="/etc/systemd/network/99-clone-$DEV.network"
    {
        echo "[Match]"
        echo "Name=$DEV"
        echo ""
        echo "[Network]"
        echo "Address=$CIDR"
        echo "Gateway=$GW"
        for d in $DNS; do echo "DNS=$d"; done
    } > "$F"
    systemctl restart systemd-networkd
}

apply_ifupdown() {
    if [ -d /etc/network/interfaces.d ] && grep -qs "interfaces.d" /etc/network/interfaces; then
        F="/etc/network/interfaces.d/99-clone-$DEV"
    else
        F=/etc/network/interfaces
    fi
    {
        echo ""
        echo "auto $DEV"
        echo "iface $DEV inet static"
        echo "    address $IP"
        echo "    netmask $NETMASK"
        echo "    gateway $GW"
        echo "    dns-nameservers $DNS"
    } >> "$F"
    ifdown "$DEV" 2>/dev/null || true
    ifup "$DEV" 2>/dev/null || systemctl restart networking 2>/dev/null || service networking restart
}

# --- 识别网络管理方式并应用 ---
if command -v netplan >/dev/null 2>&1 && [ -d /etc/netplan ]; then
    METHOD=netplan
elif command -v nmcli >/dev/null 2>&1 && systemctl is-active --quiet NetworkManager 2>/dev/null; then
    METHOD=nmcli
elif systemctl is-active --quiet systemd-networkd 2>/dev/null; then
    METHOD=networkd
elif [ -e /etc/network/interfaces ]; then
    METHOD=ifupdown
else
    echo "ERROR: 无法识别网络管理方式(nmcli/netplan/networkd/ifupdown 均不适用)"; exit 1
fi
echo "网络管理方式: $METHOD"
apply_"$METHOD"

sleep 2
echo "=== 生效后的 IP ==="
ip -4 addr show "$DEV" | grep -o 'inet [0-9.]*/[0-9]*' || true
`
