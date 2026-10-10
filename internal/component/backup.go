package component

import (
	"strconv"
	"time"

	"github.com/sebnow/orchestrator/internal/html"
)

// Backup is a backup attempt as the GUI shows it: File is empty when no
// copy was written, UploadedTo when it was not uploaded, and Error when
// nothing failed.
type Backup struct {
	At         time.Time
	Size       int64
	File       string
	UploadedTo string
	Error      string
}

// Backups shows schedule, the last backup attempt, nil before the first,
// and the button that backs up now, or only schedule when enabled is
// false.
func Backups(last *Backup, schedule string, enabled bool) html.Node {
	if !enabled {
		return html.El("p", attrs("class", "empty"), html.Text(schedule))
	}
	var outcome html.Node
	switch {
	case last == nil:
		outcome = html.El("p", attrs("class", "empty"), html.Text("No backup yet."))
	default:
		var where []html.Node
		if last.File != "" {
			where = append(where, html.Text(", "+byteSize(last.Size)+", written to "), html.El("code", nil, html.Text(last.File)))
		}
		if last.UploadedTo != "" {
			where = append(where, html.Text(" and uploaded to "), html.El("code", nil, html.Text(last.UploadedTo)))
		}
		var failure html.Node
		if last.Error != "" {
			failure = html.El("p", attrs("class", "problem backup-error"), html.Text("It failed: "+last.Error))
		}
		outcome = html.Fragment(
			html.El("p", attrs("class", "backup"), html.Text("Last backup "), timestamp(last.At), html.Fragment(where...), html.Text(".")),
			failure)
	}
	return html.Fragment(
		html.El("p", nil, html.Text(schedule)),
		outcome,
		PlainForm("/backups", "", Button("Back up now", VariantPlain, "", "")))
}

// byteSize is n bytes in the largest binary unit that keeps it at least
// 1, to one decimal place.
func byteSize(n int64) string {
	if n < 1024 {
		return strconv.FormatInt(n, 10) + " bytes"
	}
	size := float64(n)
	unit := ""
	for _, next := range []string{"KiB", "MiB", "GiB", "TiB"} {
		size /= 1024
		unit = next
		if size < 1024 {
			break
		}
	}
	return strconv.FormatFloat(size, 'f', 1, 64) + " " + unit
}
