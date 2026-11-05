package schedule

import (
	"strings"
	"testing"
	"time"
)

func TestDailyTaskXMLHasATimeAndNoRepetition(t *testing.T) {
	x, err := windowsTaskXML(Entry{Name: "r2backup", DailyAt: "21:30", BinaryPath: `C:\r2b.exe`, Args: []string{"backup"}}, `PC\me`)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(x, "<StartBoundary>2020-01-01T21:30:00</StartBoundary>") {
		t.Errorf("no 21:30 start in:\n%s", x)
	}
	if strings.Contains(x, "<Repetition>") {
		t.Errorf("a daily task must not repeat:\n%s", x)
	}
	if got := windowsParseTaskDailyAt(x); got != "21:30" {
		t.Errorf("read back %q, want 21:30", got)
	}
}

func TestIntervalTaskIsNotDaily(t *testing.T) {
	x, err := windowsTaskXML(Entry{Name: "r2backup", Interval: 45 * time.Minute, BinaryPath: `C:\r2b.exe`}, `PC\me`)
	if err != nil {
		t.Fatal(err)
	}
	if got := windowsParseTaskDailyAt(x); got != "" {
		t.Errorf("an interval task read back as daily at %q", got)
	}
	if d, err := windowsParseTaskInterval(x); err != nil || d != 45*time.Minute {
		t.Errorf("interval read back %v, %v", d, err)
	}
}

func TestParseDailyAt(t *testing.T) {
	for in, want := range map[string]string{"21:30": "21:30", "9:05": "09:05", " 00:00 ": "00:00"} {
		h, m, err := ParseDailyAt(in)
		if err != nil || FormatDailyAt(h, m) != want {
			t.Errorf("ParseDailyAt(%q) = %d:%d, %v; want %s", in, h, m, err, want)
		}
	}
	for _, in := range []string{"", "24:00", "12:60", "9", "9:5", "ab:cd"} {
		if _, _, err := ParseDailyAt(in); err == nil {
			t.Errorf("ParseDailyAt(%q) should fail", in)
		}
	}
}
