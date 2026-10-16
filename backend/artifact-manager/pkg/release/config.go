package release

import "time"

type Config struct {
	Timeout       time.Duration
	WorkTTL       time.Duration
	HistoryTTL    time.Duration
	Workers       int
	QueueCapacity int
	HistoryCount  int
	PublicKey     string
}
