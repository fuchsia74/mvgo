package main

import (
	"encoding/base64"
	"fmt"
	"net"
	"os"
	"path/filepath"
)

func b64encode(s string) string {
	return base64.StdEncoding.EncodeToString([]byte(s))
}

// shellQuote 用单引号安全包裹一个 shell 参数(等价 Python shlex.quote)。
func shellQuote(s string) string {
	if s == "" {
		return "''"
	}
	// 单引号内除 ' 外都是字面量;' 用 '\'' 转义
	return "'" + replaceAll(s, "'", `'\''`) + "'"
}

func replaceAll(s, old, new string) string {
	out := ""
	for {
		i := indexOf(s, old)
		if i < 0 {
			return out + s
		}
		out += s[:i] + new
		s = s[i+len(old):]
	}
}

func indexOf(s, sub string) int {
	for i := 0; i+len(sub) <= len(s); i++ {
		if s[i:i+len(sub)] == sub {
			return i
		}
	}
	return -1
}

func readFile(path string) ([]byte, error) {
	return os.ReadFile(path)
}

// promptPassword 从终端读一行口令(明文回显)。空则返回 ""。
// 说明:未用隐藏输入以避免引入 x/term 依赖;推荐走环境变量提供口令。
func promptPassword(prompt string) string {
	fmt.Fprint(os.Stderr, prompt)
	var line string
	if _, err := fmt.Fscanln(os.Stdin, &line); err != nil {
		return ""
	}
	return line
}

func baseName(path string) string {
	return filepath.Base(path)
}

func isFile(path string) bool {
	info, err := os.Stat(path)
	return err == nil && !info.IsDir()
}

// computeIP 起始 CIDR(如 192.168.1.100/24)按 index 递增,返回新的 "ip/prefix"。
// 保留主机位(net.ParseCIDR 的第一个返回值即为原始 IP,不被掩码清零)。
// 解析失败返回错误(用于 CIDR 格式校验)。
func computeIP(startCIDR string, index int) (string, error) {
	host, prefix, err := splitCIDR(startCIDR)
	if err != nil {
		return "", err
	}
	ip := net.ParseIP(host)
	if ip == nil || ip.To4() == nil {
		return "", fmt.Errorf("非法 IPv4 地址: %s", host)
	}
	newIP := addToIP(ip, index)
	return fmt.Sprintf("%s/%d", newIP.String(), prefix), nil
}

// splitCIDR 把 "ip/prefix" 拆成 (ip, prefix)。
func splitCIDR(s string) (string, int, error) {
	slash := indexOf(s, "/")
	if slash < 0 {
		return "", 0, fmt.Errorf("缺少 /前缀: %s", s)
	}
	host := s[:slash]
	prefixStr := s[slash+1:]
	prefix := 0
	for _, c := range prefixStr {
		if c < '0' || c > '9' {
			return "", 0, fmt.Errorf("非法前缀: %s", prefixStr)
		}
		prefix = prefix*10 + int(c-'0')
	}
	if prefix < 0 || prefix > 32 {
		return "", 0, fmt.Errorf("前缀超范围: %d", prefix)
	}
	return host, prefix, nil
}

// addToIP 把 IPv4 地址加上 index(用于按序号递增)。
func addToIP(ip net.IP, index int) net.IP {
	v4 := ip.To4()
	if v4 == nil {
		return ip
	}
	val := uint32(v4[0])<<24 | uint32(v4[1])<<16 | uint32(v4[2])<<8 | uint32(v4[3])
	val += uint32(index)
	out := make(net.IP, 4)
	out[0] = byte(val >> 24)
	out[1] = byte(val >> 16)
	out[2] = byte(val >> 8)
	out[3] = byte(val)
	return out
}

// ipHost 从 "ip/prefix" 取出 ip 部分。
func ipHost(cidr string) string {
	if i := indexOf(cidr, "/"); i >= 0 {
		return cidr[:i]
	}
	return cidr
}
