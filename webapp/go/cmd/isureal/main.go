package main

import (
	"fmt"
	"net/http"
	_ "net/http/pprof"
	"os"

	"example.com/isureal-company/isureal"
)

// 2026-09-23

func main() {
	// isu1 が初期化のときに ssh で呼ぶ。画像の署名だけを計算して返す（image_presign.go）。
	if len(os.Args) > 1 && os.Args[1] == "presign-worker" {
		if err := isureal.RunPresignWorker(os.Stdin, os.Stdout); err != nil {
			fmt.Fprintln(os.Stderr, "presign-worker:", err)
			os.Exit(1)
		}
		return
	}
	go func() {
		_ = http.ListenAndServe("127.0.0.1:6060", nil)
	}()
	config := isureal.ConfigFromEnv()
	server := isureal.NewServer(config)
	if err := server.Start(":8080"); err != nil {
		panic("failed to start server: " + err.Error())
	}
}
