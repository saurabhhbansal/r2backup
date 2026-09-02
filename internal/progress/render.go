package progress

import (
	"math"
	"strconv"
	"strings"
	"time"
)

// This file is deliberately separate from tracker.go: the model (rates,
// ETA, damping) and the presentation (bar characters, unit labels, plural
// "sec"/"min") are independently testable, and a display tweak here should
// never risk touching the estimation math.

// defaultBarWidth is used when Render is called with width <= 0.
const defaultBarWidth = 30

const (
	filledRune = '█'
	emptyRune  = '░'
)

// Render draws a two-line terminal progress display for s:
//
//	████████████████░░░░░░░░  63%   214/340 MB   1,166/1,847 files
//	18.4 MB/s · 94 files/s · 7 min 12 sec remaining
//
// width is the bar's character width; width <= 0 uses a sensible default.
func Render(s Snapshot, width int) string {
	if width <= 0 {
		width = defaultBarWidth
	}

	var b strings.Builder
	b.WriteString(bar(s.Percent, width))
	b.WriteString("  ")
	b.WriteString(strconv.Itoa(int(math.Round(clamp01to100(s.Percent)))))
	b.WriteString("%   ")
	b.WriteString(formatBytesPair(s.BytesDone, s.BytesTotal))
	b.WriteString("   ")
	b.WriteString(formatCountPair(s.FilesDone, s.FilesTotal))
	b.WriteString("\n")

	b.WriteString(FormatRate(s.ByteRate))
	b.WriteString(" · ")
	b.WriteString(FormatFileRate(s.FileRate))
	b.WriteString(" · ")
	if s.ETAKnown {
		b.WriteString(FormatDuration(s.ETA))
		b.WriteString(" remaining")
	} else {
		b.WriteString("estimating…")
	}

	return b.String()
}

func clamp01to100(p float64) float64 {
	if p < 0 {
		return 0
	}
	if p > 100 {
		return 100
	}
	return p
}

func bar(percent float64, width int) string {
	filled := int(math.Round(clamp01to100(percent) / 100 * float64(width)))
	if filled < 0 {
		filled = 0
	}
	if filled > width {
		filled = width
	}
	var b strings.Builder
	b.Grow(width * 3) // multi-byte runes
	for i := 0; i < filled; i++ {
		b.WriteRune(filledRune)
	}
	for i := filled; i < width; i++ {
		b.WriteRune(emptyRune)
	}
	return b.String()
}

// byteUnits mirrors the 1024-based convention used throughout this project
// (rclone, which this tool replaces, reports the same way), labelled with
// the short SI-looking names users actually read ("MB" not "MiB").
var byteUnits = []string{"B", "KB", "MB", "GB", "TB", "PB"}

// splitUnit picks the largest unit in which n is >= 1 (or B, for anything
// under 1024), returning the scaled value and its label.
func splitUnit(n int64) (float64, string) {
	if n <= 0 {
		return 0, byteUnits[0]
	}
	v := float64(n)
	unit := 0
	for v >= 1024 && unit < len(byteUnits)-1 {
		v /= 1024
		unit++
	}
	return v, byteUnits[unit]
}

// FormatBytes renders a byte count as a human-readable size: whole bytes
// below 1 KiB ("999 B"), otherwise one decimal place with a trailing ".0"
// trimmed for a clean whole number ("214 MB") but kept when it carries real
// information ("1.5 GB").
func FormatBytes(n int64) string {
	if n < 0 {
		n = 0
	}
	v, unit := splitUnit(n)
	if unit == "B" {
		return strconv.FormatInt(int64(v), 10) + " B"
	}
	return trimDecimal(v) + " " + unit
}

// trimDecimal formats to one decimal place and drops a trailing ".0".
func trimDecimal(v float64) string {
	s := strconv.FormatFloat(v, 'f', 1, 64)
	return strings.TrimSuffix(s, ".0")
}

// formatBytesPair renders "done/total UNIT" using a single shared unit
// chosen from the total, so the two numbers stay directly comparable
// instead of each picking its own scale.
func formatBytesPair(done, total int64) string {
	if total < 0 {
		total = 0
	}
	if done < 0 {
		done = 0
	}
	_, unit := splitUnit(total)
	if total <= 0 {
		// Nothing to scale against; fall back to done's own unit so a
		// zero-byte-total plan still renders something sane.
		_, unit = splitUnit(done)
	}
	return scaledIn(done, unit) + "/" + scaledIn(total, unit) + " " + unit
}

// scaledIn formats n in the given fixed unit (one of byteUnits).
func scaledIn(n int64, unit string) string {
	idx := 0
	for i, u := range byteUnits {
		if u == unit {
			idx = i
			break
		}
	}
	v := float64(n)
	for i := 0; i < idx; i++ {
		v /= 1024
	}
	if unit == "B" {
		return strconv.FormatInt(int64(v), 10)
	}
	return trimDecimal(v)
}

// FormatRate renders a bytes-per-second EWMA value, always to one decimal
// place (e.g. "18.4 MB/s") since a rate's precision matters more than a
// clean whole number the way a static total's does.
func FormatRate(bytesPerSecond float64) string {
	if bytesPerSecond < 0 || math.IsNaN(bytesPerSecond) || math.IsInf(bytesPerSecond, 0) {
		bytesPerSecond = 0
	}
	v, unit := splitUnit(int64(math.Round(bytesPerSecond)))
	if unit == "B" {
		return strconv.FormatFloat(v, 'f', 1, 64) + " B/s"
	}
	return strconv.FormatFloat(v, 'f', 1, 64) + " " + unit + "/s"
}

// FormatFileRate renders a files-per-second EWMA value as a rounded whole
// number with thousands separators ("94 files/s") -- fractional files per
// second are not a meaningful thing to show a user.
func FormatFileRate(filesPerSecond float64) string {
	if filesPerSecond < 0 || math.IsNaN(filesPerSecond) || math.IsInf(filesPerSecond, 0) {
		filesPerSecond = 0
	}
	return FormatCount(int64(math.Round(filesPerSecond))) + " files/s"
}

// FormatCount renders an integer with thousands separators ("1,847").
// Hand-rolled because this package uses only the standard library.
func FormatCount(n int64) string {
	neg := n < 0
	if neg {
		n = -n
	}
	s := strconv.FormatInt(n, 10)
	var out strings.Builder
	out.Grow(len(s) + len(s)/3)
	lead := len(s) % 3
	if lead == 0 {
		lead = 3
	}
	out.WriteString(s[:lead])
	for i := lead; i < len(s); i += 3 {
		out.WriteByte(',')
		out.WriteString(s[i : i+3])
	}
	res := out.String()
	if neg {
		res = "-" + res
	}
	return res
}

// formatCountPair renders "done/total files".
func formatCountPair(done, total int64) string {
	return FormatCount(done) + "/" + FormatCount(total) + " files"
}

// FormatDuration renders a duration the way a person reads a countdown:
// the two most significant units, dropping seconds once the duration
// reaches an hour ("2 hr 3 min") because at that scale seconds are noise,
// and dropping minutes once it is under a minute ("45 sec").
func FormatDuration(d time.Duration) string {
	if d < 0 {
		d = 0
	}
	total := int64(d.Round(time.Second) / time.Second)

	hours := total / 3600
	minutes := (total % 3600) / 60
	seconds := total % 60

	switch {
	case hours > 0:
		return strconv.FormatInt(hours, 10) + " hr " + strconv.FormatInt(minutes, 10) + " min"
	case minutes > 0:
		return strconv.FormatInt(minutes, 10) + " min " + strconv.FormatInt(seconds, 10) + " sec"
	default:
		return strconv.FormatInt(seconds, 10) + " sec"
	}
}

// FormatInterval says how often something recurring happens, in words:
// "30 minutes", "1 hour", "1 hour 30 minutes".
//
// time.Duration.String() renders half an hour as "30m0s", which is a
// programmer's spelling of it: correct, and not what anybody would say out
// loud. This is read in a sentence -- "every ..." -- so it has to finish
// that sentence the way a person would. Unlike FormatDuration above, this is
// never read against a countdown, so it never shows seconds: nobody
// describes how often a recurring job runs to single-second precision, and
// a caller with an interval that cannot honestly be rounded to a whole
// minute (in particular, a non-positive one -- see the callers in
// internal/cli/commands.go) should say so itself rather than calling this.
func FormatInterval(d time.Duration) string {
	d = d.Round(time.Minute)
	h, m := int(d/time.Hour), int(d%time.Hour/time.Minute)
	switch {
	case h > 0 && m > 0:
		return pluralWord(h, "hour") + " " + pluralWord(m, "minute")
	case h > 0:
		return pluralWord(h, "hour")
	case m > 0:
		return pluralWord(m, "minute")
	default:
		return "minute"
	}
}

// pluralWord renders a count with an English noun, singular or plural
// ("1 hour", "2 hours").
func pluralWord(n int, noun string) string {
	if n == 1 {
		return "1 " + noun
	}
	return strconv.Itoa(n) + " " + noun + "s"
}
