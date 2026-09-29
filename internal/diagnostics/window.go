package diagnostics

import (
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"

	"github.com/dbarena/benchctl/internal/engine"
)

// LoadStepWindows reads the step_windows.json that `benchctl fetch` copied off
// the driver instance and converts it into collection windows.
//
// These are the only windows benchctl collects over. There is deliberately no
// fallback to the run's own span: that span includes provisioning and driver
// setup, which for a cloud target is minutes of idle time, and attributing a
// Performance Insights slice to the wrong interval is worse than collecting
// nothing. A run whose windows are unavailable is a run whose driver instance
// is already gone, and fetching after teardown was never supported.
//
// A missing file is not an error. The run may predate step windows, or have
// used the local driver, which leaves the file in its own working directory
// rather than shipping it anywhere.
func LoadStepWindows(dir string) ([]Window, error) {
	path := filepath.Join(dir, engine.StepWindowsFilename)
	data, err := os.ReadFile(path)
	if errors.Is(err, fs.ErrNotExist) {
		return nil, nil
	}
	if err != nil {
		return nil, fmt.Errorf("read %s: %w", path, err)
	}
	var steps []engine.StepWindow
	if err := json.Unmarshal(data, &steps); err != nil {
		return nil, fmt.Errorf("parse %s: %w", path, err)
	}

	windows := make([]Window, 0, len(steps))
	for _, s := range steps {
		// A failed step's window is still worth collecting over; that is when
		// the provider's view matters most. A zero-length one is not: every
		// monitoring API either errors or returns nothing for an empty range.
		if !s.EndedAt.After(s.StartedAt) {
			continue
		}
		windows = append(windows, Window{
			Name:      s.Step,
			Benchmark: s.Benchmark,
			Iteration: s.Iteration,
			Fixture:   s.Fixture,
			Start:     s.StartedAt,
			End:       s.EndedAt,
		})
	}
	return windows, nil
}
