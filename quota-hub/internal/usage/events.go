package usage

import (
	"bufio"
	"encoding/json"
	"io"
	"os"
	"path/filepath"
	"time"
)

type Tokens struct {
	Input      int64 `json:"input"`
	Output     int64 `json:"output"`
	Reasoning  int64 `json:"reasoning"`
	CacheRead  int64 `json:"cacheRead"`
	CacheWrite int64 `json:"cacheWrite"`
}

type Event struct {
	V        int      `json:"v"`
	ID       string   `json:"id"`
	Time     int64    `json:"time"`
	Source   string   `json:"source"`
	Status   string   `json:"status"`
	Session  string   `json:"session"`
	Message  *string  `json:"message"`
	Provider *string  `json:"provider"`
	Model    *string  `json:"model"`
	Variant  *string  `json:"variant"`
	Agent    *string  `json:"agent"`
	Tokens   Tokens   `json:"tokens"`
	Cost     *float64 `json:"cost"`
}

func parseEvent(line []byte) (Event, bool) {
	var e Event
	var required struct {
		Time   *int64            `json:"time"`
		Tokens map[string]*int64 `json:"tokens"`
	}
	if json.Unmarshal(line, &e) != nil || json.Unmarshal(line, &required) != nil || e.V != 1 || e.ID == "" || required.Time == nil {
		return e, false
	}
	for _, key := range []string{"input", "output", "reasoning", "cacheRead", "cacheWrite"} {
		if n := required.Tokens[key]; n == nil || *n < 0 {
			return e, false
		}
	}
	return e, true
}

func BaseDir() string {
	if x := os.Getenv("XDG_STATE_HOME"); filepath.IsAbs(x) {
		return filepath.Join(x, "quota-usage")
	}
	home, _ := os.UserHomeDir()
	return filepath.Join(home, ".local", "state", "quota-usage")
}

// readLines never consumes a trailing, incomplete record.
func readLines(path string, offset int64, visit func([]byte) error) (int64, error) {
	f, err := os.Open(path)
	if err != nil {
		return offset, err
	}
	defer f.Close()
	if _, err = f.Seek(offset, io.SeekStart); err != nil {
		return offset, err
	}
	r := bufio.NewReader(f)
	for {
		line, err := r.ReadBytes('\n')
		if err == io.EOF {
			return offset, nil
		}
		if err != nil {
			return offset, err
		}
		if err = visit(line); err != nil {
			return offset, err
		}
		offset += int64(len(line))
	}
}

func day(ms int64) string { return time.UnixMilli(ms).UTC().Format("2006-01-02") }
