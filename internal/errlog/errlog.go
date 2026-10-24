// Package errlog appends failures that must not interrupt the user, such
// as a hook's internal error, to StateDir/errors.log (SPEC §17). Entries
// carry no secrets: callers pass errors whose text is already safe to show.
package errlog

import (
	"fmt"
	"os"
	"path/filepath"
	"time"

	"github.com/muratgozel/seasoning/internal/paths"
)

// maxSize caps errors.log; past it the file starts over.
const maxSize = 1 << 20

// Logger appends to one errors.log.
type Logger struct{ Path string }

// Default returns the logger for StateDir/errors.log.
func Default() Logger {
	d, err := paths.StateDir()
	if err != nil {
		return Logger{}
	}
	return Logger{Path: filepath.Join(d, "errors.log")}
}

// Log appends one line: UTC time, where, and the error.
func (l Logger) Log(where string, err error) {
	if l.Path == "" || err == nil {
		return
	}
	if info, statErr := os.Stat(l.Path); statErr == nil && info.Size() > maxSize {
		os.Remove(l.Path)
	}
	os.MkdirAll(filepath.Dir(l.Path), 0o700)
	f, ferr := os.OpenFile(l.Path, os.O_CREATE|os.O_APPEND|os.O_WRONLY, 0o600)
	if ferr != nil {
		return
	}
	defer f.Close()
	fmt.Fprintf(f, "%s %s: %v\n", time.Now().UTC().Format(time.RFC3339), where, err)
}
