package main

import (
	"fmt"
	"os"
)

func main() {
	if len(os.Args) == 1 || (len(os.Args) == 2 && (os.Args[1] == "help" || os.Args[1] == "-h" || os.Args[1] == "--help")) {
		printUsage()
		return
	}
	fmt.Fprintf(os.Stderr, "未知命令: %s\n", os.Args[1])
	os.Exit(1)
}

func printUsage() {
	fmt.Println("QDrive - 夸克网盘命令行工具")
	fmt.Println("用法: qdrive [help]")
}
