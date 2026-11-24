package main

import (
	"os"

	"k8s.io/klog/v2"

	"ebs-gateway/cmd/app"
)

func main() {
	if err := app.NewServerCommand().Execute(); err != nil {
		klog.ErrorS(err, "Gateway failed")
		klog.Flush()
		os.Exit(1)
	}
	klog.Flush()
}
