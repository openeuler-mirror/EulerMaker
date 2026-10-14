package main

import (
	genericapiserver "k8s.io/apiserver/pkg/server"
	"k8s.io/component-base/logs"
	"k8s.io/klog/v2"

	"ebs-apiserver/pkg/server"
)

func main() {
	logs.InitLogs()
	defer logs.FlushLogs()

	stopCh := genericapiserver.SetupSignalHandler()
	if err := server.Run(stopCh); err != nil {
		klog.Fatalf("ebs-apiserver failed: %v", err)
	}
}
