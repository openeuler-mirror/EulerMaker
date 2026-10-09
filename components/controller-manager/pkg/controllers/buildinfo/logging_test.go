package buildinfo

import (
	"bytes"
	"flag"
	"log"
	"strings"
	"testing"

	"k8s.io/klog/v2"
)

func TestBreakPointLogVerbosity(t *testing.T) {
	flags := flag.NewFlagSet("logging-test", flag.ContinueOnError)
	klog.InitFlags(flags)
	previous := flags.Lookup("v").Value.String()
	output := log.Writer()
	t.Cleanup(func() {
		_ = flags.Set("v", previous)
		log.SetOutput(output)
	})
	for _, verbosity := range []string{"0", "4"} {
		t.Run(verbosity, func(t *testing.T) {
			if err := flags.Set("v", verbosity); err != nil {
				t.Fatal(err)
			}
			for _, reason := range []string{"BootstrapBreaks", "RuntimeBootstrapBreaks"} {
				var buf bytes.Buffer
				log.SetOutput(&buf)
				(&Controller{}).logBreakPoints(
					"project/build",
					reason,
					"nodes=4950 break_count=2",
					[]string{"SDL2", "antlr4"},
				)
				got := buf.String()
				if !strings.Contains(got, "nodes=4950 break_count=2") || !strings.Contains(got, "reason="+reason) {
					t.Fatalf("missing summary: %s", got)
				}
				if strings.Contains(got, "SDL2,antlr4") != (verbosity == "4") {
					t.Fatalf("unexpected details at verbosity %s: %s", verbosity, got)
				}
			}
		})
	}
}
