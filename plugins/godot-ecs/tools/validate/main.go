// Command validate 对 plugin.yaml 跑语义契约校验（runtime + schema + state 三层）。
// 输出 "contract-conformant" 表示注册时宿主校验会通过。
package main

import (
	"fmt"
	"os"

	"github.com/OwnSecurityGuard/gta-plugin-sdk/contract"
)

func main() {
	data, err := os.ReadFile("plugin.yaml")
	if err != nil {
		fmt.Println("FAIL:", err)
		os.Exit(1)
	}
	if err := contract.CheckManifest(data); err != nil {
		fmt.Println("FAIL:", err)
		os.Exit(1)
	}
	fmt.Println("contract-conformant")
}
