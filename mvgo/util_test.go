package main

import (
	"os"
	"strings"
	"testing"
)

func TestComputeIP(t *testing.T) {
	cases := []struct {
		start string
		idx   int
		want  string
	}{
		{"192.168.1.100/24", 0, "192.168.1.100/24"},
		{"192.168.1.100/24", 4, "192.168.1.104/24"},
		{"10.0.0.7/16", 0, "10.0.0.7/16"},
		{"10.0.0.250/24", 10, "10.0.1.4/24"}, // 跨段进位
	}
	for _, c := range cases {
		got, err := computeIP(c.start, c.idx)
		if err != nil {
			t.Fatalf("computeIP(%q,%d) err: %v", c.start, c.idx, err)
		}
		if got != c.want {
			t.Errorf("computeIP(%q,%d)=%q, want %q", c.start, c.idx, got, c.want)
		}
	}
	if _, err := computeIP("not-an-ip", 0); err == nil {
		t.Error("computeIP 应对非法输入报错")
	}
}

func TestPrefixToNetmask(t *testing.T) {
	cases := map[int]string{24: "255.255.255.0", 16: "255.255.0.0", 8: "255.0.0.0", 30: "255.255.255.252"}
	for p, want := range cases {
		got, err := prefixToNetmask(p)
		if err != nil || got != want {
			t.Errorf("prefixToNetmask(%d)=%q(err=%v), want %q", p, got, err, want)
		}
	}
}

func TestShellQuote(t *testing.T) {
	cases := map[string]string{
		"":            "''",
		"abc":         "'abc'",
		"a b":         "'a b'",
		"it's":        `'it'\''s'`,
		"192.168.1.1": "'192.168.1.1'",
	}
	for in, want := range cases {
		if got := shellQuote(in); got != want {
			t.Errorf("shellQuote(%q)=%q, want %q", in, got, want)
		}
	}
}

func TestSpaceJoinDNS(t *testing.T) {
	if got := spaceJoinDNS("8.8.8.8,1.1.1.1"); got != "8.8.8.8 1.1.1.1" {
		t.Errorf("got %q", got)
	}
	if got := spaceJoinDNS("8.8.8.8"); got != "8.8.8.8" {
		t.Errorf("got %q", got)
	}
}

// TestBuildNetcfgScript 校验生成的脚本含关键结构,并写出供 bash -n 语法检查。
func TestBuildNetcfgScript(t *testing.T) {
	s, err := buildNetcfgScript("192.168.1.105/24", "192.168.1.1", "8.8.8.8,1.1.1.1", "", "")
	if err != nil {
		t.Fatal(err)
	}
	for _, must := range []string{
		"IP='192.168.1.105'", "PREFIX='24'", "NETMASK='255.255.255.0'",
		"GW='192.168.1.1'", "DNS='8.8.8.8 1.1.1.1'",
		"apply_netplan", "apply_nmcli", "apply_networkd", "apply_ifupdown",
		"command -v netplan", "systemd-networkd",
	} {
		if !strings.Contains(s, must) {
			t.Errorf("生成脚本缺少 %q", must)
		}
	}
	// 写到临时文件供外部 bash -n 检查
	if p := os.Getenv("NETCFG_DUMP"); p != "" {
		os.WriteFile(p, []byte(s), 0644)
	}
}
