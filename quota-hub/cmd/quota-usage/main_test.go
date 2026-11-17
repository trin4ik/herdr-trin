package main

import (
	"encoding/json"
	"flag"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func invoke(t *testing.T, args ...string) (string, error) {
	t.Helper()
	oldArgs, oldFlags, oldOut := os.Args, flag.CommandLine, os.Stdout
	f, err := os.CreateTemp(t.TempDir(), "stdout")
	if err != nil {
		t.Fatal(err)
	}
	defer f.Close()
	os.Args = append([]string{"quota-usage"}, args...)
	flag.CommandLine = flag.NewFlagSet("quota-usage", flag.ContinueOnError)
	os.Stdout = f
	defer func() { os.Args, flag.CommandLine, os.Stdout = oldArgs, oldFlags, oldOut }()
	err = run()
	b, readErr := os.ReadFile(f.Name())
	if readErr != nil {
		t.Fatal(readErr)
	}
	return string(b), err
}

func TestCLI(t *testing.T) {
	dir := t.TempDir()
	out, err := invoke(t, "-dir", dir, "-offline", "-once")
	if err != nil || !json.Valid([]byte(out)) || !strings.Contains(out, `"ok":true`) {
		t.Fatalf("%s %v", out, err)
	}
	saved, err := invoke(t, "-dir", dir, "-summary")
	if err != nil || saved != out {
		t.Fatalf("%s %v", saved, err)
	}
	out, err = invoke(t, "-dir", dir, "-report", "-from", "2026-11-17T00:00:00Z", "-to", "2026-11-18T00:00:00Z")
	if err != nil || !strings.Contains(out, `"calls":0`) {
		t.Fatalf("%s %v", out, err)
	}
	for _, args := range [][]string{{"-interval", "0"}, {"-report", "-from", "bad"}, {"-report", "-from", "2026-11-18T00:00:00Z", "-to", "2026-11-17T00:00:00Z"}} {
		if _, err = invoke(t, append([]string{"-dir", dir}, args...)...); err == nil {
			t.Fatalf("accepted %v", args)
		}
	}
	if err = os.WriteFile(filepath.Join(dir, "state.json"), []byte("broken"), 0600); err != nil {
		t.Fatal(err)
	}
	out, err = invoke(t, "-dir", dir, "-offline", "-once")
	if err == nil || !strings.Contains(out, `"ok":false`) {
		t.Fatalf("%s %v", out, err)
	}
}
