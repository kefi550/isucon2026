package main

import (
	"net/http"
	_ "net/http/pprof"

	"example.com/isureal-company/isureal"
)

// 2026-09-23

func main() {
	go func() {
		_ = http.ListenAndServe("127.0.0.1:6060", nil)
	}()
	config := isureal.ConfigFromEnv()
	server := isureal.NewServer(config)
	if err := server.Start(":8080"); err != nil {
		panic("failed to start server: " + err.Error())
	}
}
