package container

import (
	"context"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
)

func TestDockerCLIRecoveryQueries(t *testing.T) {
	dir := t.TempDir()
	argsPath := filepath.Join(dir, "args")
	script := `#!/bin/sh
printf '%s\n' "$@" > "$DOCKER_ARGS_FILE"
case "$1" in
ps) printf 'container-a\ncontainer-b\n' ;;
inspect) printf '{"ebs.io/runner":"runner-a","ebs.io/project":"project","ebs.io/job":"job","ebs.io/job-uid":"uid"}\n' ;;
esac
`
	if err := os.WriteFile(filepath.Join(dir, "docker"), []byte(script), 0o755); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PATH", dir+string(os.PathListSeparator)+os.Getenv("PATH"))
	t.Setenv("DOCKER_ARGS_FILE", argsPath)
	readArgs := func() []string {
		t.Helper()
		data, err := os.ReadFile(argsPath)
		if err != nil {
			t.Fatal(err)
		}
		return strings.Split(strings.TrimSpace(string(data)), "\n")
	}
	runtime := DockerCLI{}
	ids, err := runtime.List(context.Background(), map[string]string{
		"ebs.io/runner":  "runner-a",
		"ebs.io/job-uid": "",
	})
	if err != nil || !reflect.DeepEqual(ids, []string{"container-a", "container-b"}) {
		t.Fatalf("List = %v, %v", ids, err)
	}
	args := readArgs()
	if len(args) != 6 || args[0] != "ps" || args[1] != "-aq" || args[2] != "--filter" || args[4] != "--filter" {
		t.Fatalf("list args = %v", args)
	}
	filters := map[string]bool{args[3]: true, args[5]: true}
	if !filters["label=ebs.io/runner=runner-a"] || !filters["label=ebs.io/job-uid"] {
		t.Fatalf("list filters = %v", filters)
	}
	labels, err := runtime.Labels(context.Background(), "container-a")
	if err != nil || labels["ebs.io/runner"] != "runner-a" || labels["ebs.io/job-uid"] != "uid" {
		t.Fatalf("Labels = %v, %v", labels, err)
	}
	if args := readArgs(); !reflect.DeepEqual(args, []string{"inspect", "--format", "{{json .Config.Labels}}", "container-a"}) {
		t.Fatalf("inspect args = %v", args)
	}
}
