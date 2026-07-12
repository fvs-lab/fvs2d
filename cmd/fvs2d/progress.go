package main

import "bytes"

// lineWriter adapts an io.Writer sink to fvs2's line-oriented verbose output
// ("hashing: <path>\n" from repo.CommitContext, "restoring: <path>\n" from
// repo.RestoreContext): each complete line is handed to onLine as soon as it
// arrives, so CommitStream/RestoreStream can turn per-file progress lines
// into Progress messages without buffering the whole operation's output.
// This is the "real progress" path the audit asked for; if fvs2's verbose
// format ever stops being one-line-per-file, onLine simply stops firing and
// the stream still gets its start/done Progress messages.
type lineWriter struct {
	buf    []byte
	onLine func(line string)
}

func (w *lineWriter) Write(p []byte) (int, error) {
	w.buf = append(w.buf, p...)
	for {
		i := bytes.IndexByte(w.buf, '\n')
		if i < 0 {
			break
		}
		line := string(w.buf[:i])
		w.buf = w.buf[i+1:]
		if w.onLine != nil {
			w.onLine(line)
		}
	}
	return len(p), nil
}
