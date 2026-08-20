package main

import (
	"os"
	"os/signal"
)

func signalNotify(channel chan<- os.Signal, signals ...os.Signal) {
	signal.Notify(channel, signals...)
}

func signalStop(channel chan<- os.Signal) {
	signal.Stop(channel)
}
