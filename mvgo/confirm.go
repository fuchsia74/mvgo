package main

import (
	"bufio"
	"fmt"
	"os"
	"strings"
	"sync"
)

// confirm 危险操作二次确认。skip=true(--yes)则跳过。返回是否继续。
func confirm(vms []*vmRef, actionLabel string, skip bool) bool {
	if skip {
		return true
	}
	fmt.Printf("\n即将%s以下 %d 台虚机:\n", actionLabel, len(vms))
	for _, v := range sortedByName(vms) {
		fmt.Printf("  - %s  (%s)\n", v.name(), v.powerState())
	}
	fmt.Printf("\n确认%s? 输入 yes 继续: ", actionLabel)
	reader := bufio.NewReader(os.Stdin)
	line, _ := reader.ReadString('\n')
	return strings.TrimSpace(strings.ToLower(line)) == "yes"
}

// result 单台操作结果。
type result struct {
	name string
	ok   bool
	msg  string
}

// runBatch 并发执行批量操作,汇总结果。worker 返回单台 result。
func runBatch(vms []*vmRef, workers int, actionLabel string,
	worker func(v *vmRef) result) {
	fmt.Printf("\n开始%s %d 台(并发 %d)...\n\n", actionLabel, len(vms), workers)
	if workers < 1 {
		workers = 1
	}
	sem := make(chan struct{}, workers)
	var mu sync.Mutex
	var wg sync.WaitGroup
	var ok, fail []string

	for _, v := range vms {
		wg.Add(1)
		go func(v *vmRef) {
			defer wg.Done()
			sem <- struct{}{}
			defer func() { <-sem }()
			r := worker(v)
			mu.Lock()
			mark := "✓"
			if !r.ok {
				mark = "✗"
			}
			fmt.Printf("  %s %s: %s\n", mark, r.name, r.msg)
			if r.ok {
				ok = append(ok, r.name)
			} else {
				fail = append(fail, r.name)
			}
			mu.Unlock()
		}(v)
	}
	wg.Wait()

	fmt.Printf("\n完成: 成功 %d/%d", len(ok), len(vms))
	if len(fail) > 0 {
		fmt.Printf(",失败 %d: %s\n", len(fail), strings.Join(fail, ", "))
	} else {
		fmt.Println()
	}
}

// enforceFilter:power-*/delete/customize 必须带筛选条件,否则退出。
func enforceFilter(f *filterOpts) {
	if !hasFilter(f) {
		die("该操作必须指定筛选条件(--prefix/--names/--all/--state),防止误操作。")
	}
}

// ensureNonEmpty:选中为空时提示并正常退出(非错误)。
func ensureNonEmpty(vms []*vmRef) {
	if len(vms) == 0 {
		fmt.Println("(无匹配的虚机,未执行任何操作)")
		os.Exit(0)
	}
}
