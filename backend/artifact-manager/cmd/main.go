package main

import (
	"os"

	"k8s.io/klog/v2"

	"artifact-manager/cmd/app"
	"artifact-manager/pkg/diagnostics"
)

func main() {
	if err := app.NewServerCommand().Execute(); err != nil {
		klog.ErrorS(nil, "Artifact Manager stopped", "cause", diagnostics.Cause(err))
		klog.Flush()
		os.Exit(1)
	}
	klog.Flush()
}
