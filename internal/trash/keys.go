package trash

import (
	"crypto/rand"
	"encoding/hex"
	"fmt"
	"path"
	"regexp"
	"strconv"
	"strings"
	"time"
)

// Layout of a set's prefix in the bucket: the live mirror under current/,
// and what has been moved aside under trash/<date>/, dated by the day it
// was moved rather than the day it will expire -- a person looking at the
// tree sees when something disappeared, not a countdown to when it will
// finish disappearing.
const (
	currentDir = "current"
	trashDir   = "trash"
)

// dateLayout is calendar-day resolution only. That makes Prune's cutoff
// comparison a plain string/time compare, and it makes the trash tree
// read, in a bucket browser, as a list of days rather than a wall of
// timestamps.
const dateLayout = "2006-01-02"

// liveKey is the object key for relPath in the live mirror.
func liveKey(prefix, relPath string) string {
	return path.Join(prefix, currentDir, relPath)
}

// disambigSep introduces the part of a trash basename that keeps a second
// trashing of the same relPath, on the same day, from landing on top of
// the first.
const disambigSep = "~"

// trashSuffixRE recognizes a basename this package produced:
// "<name>~<HHMMSS>-<hex>[.ext]". It is what parseTrashKey uses to recover
// the original name and, critically, the original extension.
var trashSuffixRE = regexp.MustCompile(`^(.*)` + disambigSep + `(\d{6})-([0-9a-f]+)(\.[^.]*)?$`)

// randSuffix returns a short random hex string used to disambiguate two
// objects trashed under the same relative path on the same day (e.g.
// edited at 09:00 and again at 15:00 -- both need to survive). A
// timestamp alone very rarely collides in practice, but tests run against
// a fixed, injected Clock where "now" is identical on every call, and
// under a frozen clock a timestamp alone would collide every time; a
// random component removes that dependence on either wall-clock
// resolution or any shared, mutable counter, so two goroutines calling
// Move concurrently need no coordination to avoid colliding with each
// other either.
func randSuffix() string {
	var b [6]byte
	if _, err := rand.Read(b[:]); err != nil {
		// crypto/rand failing on a real OS is essentially unreachable.
		// Falling back to the clock keeps the disambiguator from ever
		// being empty rather than risking a silent overwrite.
		return strconv.FormatInt(time.Now().UnixNano(), 16)
	}
	return hex.EncodeToString(b[:])
}

// buildTrashKey returns the dated trash key for relPath, moved at now.
//
// The disambiguator goes on the basename, before the extension --
// "report~143022-a1b2c3d4e5f6.pdf", never "report.pdf~143022-...". A
// prior bug in this codebase's sister project (r2sync) appended a conflict
// suffix after the extension and made the original filename
// unreconstructable by anything that trusted the extension to be the last
// "." in the name -- splitext, a person scanning the bucket by eye, or
// this package's own parseTrashKey. Keeping the real extension last means
// a trash listing still sorts and filters by file type exactly like the
// live tree does.
func buildTrashKey(prefix, relPath string, now time.Time) string {
	date := now.UTC().Format(dateLayout)
	dir, base := path.Split(relPath)
	ext := path.Ext(base)
	name := strings.TrimSuffix(base, ext)
	disambiguated := fmt.Sprintf("%s%s%s-%s%s", name, disambigSep, now.UTC().Format("150405"), randSuffix(), ext)
	return path.Join(prefix, trashDir, date, dir, disambiguated)
}

// parseTrashKey recovers the date directory, the original relative path,
// and -- when the basename carries one -- the HHMMSS time of day from a
// key produced by buildTrashKey.
//
// timeOfDay is the six-digit clock time straight out of the disambiguator
// (e.g. "143022"), the same value buildTrashKey wrote next to the random
// suffix. It comes back "" when the basename doesn't match
// trashSuffixRE -- a foreign object someone else put under trash/, or a
// layout from a future version of this format -- because in that case no
// time of day was ever recorded and callers must not invent one.
//
// It reports ok=false for anything not shaped like trash/<date>/... at
// all. Prune and List both need to tolerate that without panicking or,
// worse, misfiling something they don't recognize.
func parseTrashKey(prefix, key string) (date, relPath, timeOfDay string, ok bool) {
	root := path.Join(prefix, trashDir) + "/"
	if !strings.HasPrefix(key, root) {
		return "", "", "", false
	}
	rest := strings.TrimPrefix(key, root)
	slash := strings.Index(rest, "/")
	if slash < 0 {
		return "", "", "", false // a key directly under trash/, not trash/<date>/...
	}
	date = rest[:slash]
	if _, err := time.Parse(dateLayout, date); err != nil {
		return "", "", "", false
	}
	underDate := rest[slash+1:]
	dir, base := path.Split(underDate)

	m := trashSuffixRE.FindStringSubmatch(base)
	if m == nil {
		// Not a key this package's disambiguator produced. Report it
		// verbatim rather than dropping it silently -- it is still a real
		// object sitting in trash and still needs to be listable and
		// prunable, even if its "original path" is only a guess and its
		// time of day is genuinely unknown rather than merely unrecorded
		// here.
		return date, underDate, "", true
	}
	name, ext := m[1], m[4]
	return date, path.Join(dir, name+ext), m[2], true
}
