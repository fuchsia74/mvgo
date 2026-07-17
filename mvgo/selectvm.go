package main

import (
	"context"
	"fmt"
	"os"
	"sort"
	"strings"
)

// selectVMs 按筛选条件返回虚机列表。互斥优先级: names > prefix > all。
// --state 可与前三者叠加。
func (s *session) selectVMs(ctx context.Context, f *filterOpts) ([]*vmRef, error) {
	vms, err := s.allVMs(ctx)
	if err != nil {
		return nil, err
	}

	switch {
	case f.names != "":
		byName := map[string]*vmRef{}
		for _, v := range vms {
			byName[v.name()] = v
		}
		var result []*vmRef
		for _, n := range strings.Split(f.names, ",") {
			n = strings.TrimSpace(n)
			if n == "" {
				continue
			}
			if v, ok := byName[n]; ok {
				result = append(result, v)
			} else {
				fmt.Fprintf(os.Stderr, "警告: 找不到虚机 '%s',已跳过\n", n)
			}
		}
		vms = result
	case f.prefix != "":
		var result []*vmRef
		for _, v := range vms {
			if strings.HasPrefix(v.name(), f.prefix) {
				result = append(result, v)
			}
		}
		vms = result
	case f.all:
		// 全部
	default:
		// list 无条件 = 全部;power-*/delete/customize 无条件在各自入口已拦截
	}

	if f.state != "" {
		var result []*vmRef
		for _, v := range vms {
			if v.powerState() == f.state {
				result = append(result, v)
			}
		}
		vms = result
	}
	return vms, nil
}

// sortedByName 返回按名称排序的副本(不改原切片)。
func sortedByName(vms []*vmRef) []*vmRef {
	out := make([]*vmRef, len(vms))
	copy(out, vms)
	sort.Slice(out, func(i, j int) bool { return out[i].name() < out[j].name() })
	return out
}
