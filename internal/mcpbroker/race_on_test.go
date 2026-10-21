//go:build race

package mcpbroker

// raceEnabled: the race detector's instrumentation changes how much the
// dispatch path allocates, so allocation calibrations skip under it.
const raceEnabled = true
