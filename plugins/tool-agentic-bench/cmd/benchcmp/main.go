// benchcmp 比较两份 agentic-bench 报告（report.json），供 CI 判定回归。
//
// 退出码：0 = 未命中断言；1 = 命中 --fail-on-regression 或 --fail-on-new-fail；
// 2 = 用法或 IO/解析错误（与「有回归」区分，避免 CI 把坏输入当成通过）。
package main

import (
	"encoding/json"
	"flag"
	"fmt"
	"os"

	"dsc-plugin-tool-agentic-bench/internal/benchreport"
)

func main() {
	baseline := flag.String("baseline", "", "基线 report.json 路径")
	current := flag.String("current", "", "当前 report.json 路径")
	failOnRegression := flag.Bool("fail-on-regression", false, "存在回归用例时以退出码 1 结束")
	failOnNewFail := flag.Bool("fail-on-new-fail", false, "存在新增失败用例时以退出码 1 结束")
	asJSON := flag.Bool("json", false, "输出机器可读 diff")
	flag.Parse()

	if *baseline == "" || *current == "" {
		fmt.Fprintln(os.Stderr, "benchcmp: --baseline 与 --current 均为必填")
		os.Exit(2)
	}
	base, err := benchreport.Load(*baseline)
	if err != nil {
		fmt.Fprintln(os.Stderr, "benchcmp:", err)
		os.Exit(2)
	}
	cur, err := benchreport.Load(*current)
	if err != nil {
		fmt.Fprintln(os.Stderr, "benchcmp:", err)
		os.Exit(2)
	}

	diff := benchreport.DiffReports(base, cur)
	if *asJSON {
		enc := json.NewEncoder(os.Stdout)
		enc.SetIndent("", "  ")
		if err := enc.Encode(diff); err != nil {
			fmt.Fprintln(os.Stderr, "benchcmp:", err)
			os.Exit(2)
		}
	} else {
		fmt.Printf("score: %s -> %s (%s)\n", base.Score, cur.Score, diff.ScoreDelta)
		fmt.Printf("regressions: %v\n", diff.Regressions)
		fmt.Printf("improvements: %v\n", diff.Improvements)
		fmt.Printf("unchanged: %d\n", diff.Unchanged)
	}

	if (*failOnRegression && diff.HasRegression()) || (*failOnNewFail && diff.HasNewFail()) {
		os.Exit(1)
	}
}
