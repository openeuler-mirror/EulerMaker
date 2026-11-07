package main

import (
	"fmt"
	"os"

	genericapiserver "k8s.io/apiserver/pkg/server"
	"k8s.io/component-base/logs"

	"ebs-apiserver/cmd/app"
	"ebs-apiserver/cmd/mapping"
)

func main() {
	logs.InitLogs()
	err := app.NewCommand().ExecuteContext(genericapiserver.SetupSignalContext())
	code := mapping.ExitCode(err)
	if code == 1 {
		fmt.Fprintln(os.Stderr, err)
	}
	logs.FlushLogs()
	os.Exit(code)
}
