package main

import "embed"

// 控制台资源直接编进二进制：从节点遍布多个机房时，
// "主控还要带一堆静态文件"会让部署凭空多出一个出错点。
//
//go:embed dist_console.html dist_console.css dist_console.js
var consoleFS embed.FS

func consoleAsset(name string) []byte {
	data, err := consoleFS.ReadFile(name)
	if err != nil {
		return nil
	}
	return data
}
