package main

import (
	"encoding/json"
	"flag"
	"fmt"
	"os"
	"os/signal"
	"path/filepath"
	"syscall"
	"time"

	"quota-hub/internal/usage"
)

func run() error {
	dir := flag.String("dir", usage.BaseDir(), "data directory")
	once := flag.Bool("once", false, "import once")
	summary := flag.Bool("summary", false, "print saved summary")
	report := flag.Bool("report", false, "report ledger window")
	windows := flag.Bool("windows", false, "report recorded quota windows without importing")
	quotasURL := flag.String("quotas-url", "", "optional quota-hub snapshot URL (no authorization)")
	from := flag.String("from", "", "RFC3339 inclusive start")
	to := flag.String("to", "", "RFC3339 exclusive end")
	offline := flag.Bool("offline", false, "use saved prices")
	url := flag.String("models-url", "https://models.dev/api.json", "price source")
	interval := flag.Duration("interval", 5*time.Minute, "import interval")
	flag.Parse()
	if *summary {
		b, err := os.ReadFile(filepath.Join(*dir, "summary.json"))
		if err != nil {
			return err
		}
		_, err = os.Stdout.Write(b)
		return err
	}
	if *report {
		a, err := time.Parse(time.RFC3339, *from)
		if err != nil {
			return err
		}
		b, err := time.Parse(time.RFC3339, *to)
		if err != nil {
			return err
		}
		if !a.Before(b) {
			return fmt.Errorf("-from must be before -to")
		}
		r, err := usage.ReadReport(*dir, a, b)
		if err != nil {
			return err
		}
		return json.NewEncoder(os.Stdout).Encode(r)
	}
	if *windows {
		r, err := usage.ReadWindows(*dir)
		if err != nil {
			return err
		}
		return json.NewEncoder(os.Stdout).Encode(r)
	}
	if *interval <= 0 {
		return fmt.Errorf("-interval must be positive")
	}
	i, err := usage.Open(*dir)
	if err != nil {
		return err
	}
	defer i.Close()
	i.ModelsURL, i.Offline = *url, *offline
	i.QuotasURL = *quotasURL
	if *once {
		s, cycleErr := i.Cycle()
		if err := json.NewEncoder(os.Stdout).Encode(s); err != nil {
			return err
		}
		return cycleErr
	}
	signals := make(chan os.Signal, 1)
	signal.Notify(signals, syscall.SIGTERM, syscall.SIGINT)
	defer signal.Stop(signals)
	ticker := time.NewTicker(*interval)
	defer ticker.Stop()
	for {
		if _, err := i.Cycle(); err != nil {
			fmt.Fprintln(os.Stderr, err)
		}
		select {
		case <-signals:
			return nil
		case <-ticker.C:
			select {
			case <-signals:
				return nil
			default:
			}
		}
	}
}

func main() {
	if err := run(); err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
}
