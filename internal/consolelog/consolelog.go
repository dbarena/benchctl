// Package consolelog provides the shared "==>" phase-line prefix used
// throughout benchctl's console output.
package consolelog

import (
	"fmt"
	"io"
	"time"
)

const TimestampLayout = "2006-01-02 15:04:05 UTC"

// Timestamp returns the current UTC time formatted for benchctl's "==>"
// console phase-line prefix.
func Timestamp() string {
	return time.Now().UTC().Format(TimestampLayout)
}

// Println writes msg to w as a "==> <UTC timestamp> - " prefixed line. Callers
// build msg with fmt.Sprintf (using a literal format string) rather than
// passing one here, so `go vet`'s printf check still applies to it.
func Println(w io.Writer, msg string) {
	fmt.Fprintln(w, "==> "+Timestamp()+" - "+msg)
}
