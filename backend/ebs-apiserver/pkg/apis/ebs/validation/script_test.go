package validation

import (
	"strings"
	"testing"

	ebsv1 "ebs-api/ebs/v1"
)

func TestValidateScript(t *testing.T) {
	for _, tc := range []struct {
		name, content string
		valid         bool
	}{
		{"shell", "#!/bin/sh\necho hello\n", true},
		{"env", "#!/usr/bin/env python3\nprint('hello')\n", true},
		{"empty", "", false},
		{"missing shebang", "echo hello", false},
		{"relative interpreter", "#!bash\necho hello", false},
		{"empty interpreter", "#!\n", false},
		{"directory interpreter", "#!/\n", false},
		{"CRLF", "#!/bin/bash\r\n", false},
		{"NUL", "#!/bin/sh\n\x00", false},
		{"invalid UTF8", "#!/bin/sh\n\xff", false},
		{"larger than 256 KiB", "#!/bin/sh\n" + strings.Repeat("#", 512*1024), true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			obj := &ebsv1.Script{Spec: ebsv1.ScriptSpec{Content: tc.content}}
			obj.Name = "rpmbuild"
			if errs := ValidateScript(obj); (len(errs) == 0) != tc.valid {
				t.Fatalf("valid=%v errors=%v", tc.valid, errs)
			}
		})
	}
	obj := &ebsv1.Script{Spec: ebsv1.ScriptSpec{Content: "#!/bin/sh\n"}}
	obj.Name = "rpmbuild"
	obj.Namespace, obj.GenerateName = "project", "script-"
	if errs := ValidateScript(obj); len(errs) != 2 {
		t.Fatalf("errors=%v", errs)
	}
}
