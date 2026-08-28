package progress

import (
	"strings"
	"testing"
	"time"
)

func TestFormatBytesEdgeCases(t *testing.T) {
	cases := []struct {
		n    int64
		want string
	}{
		{0, "0 B"},
		{999, "999 B"},
		{1023, "1023 B"},
		{1024 * 1024, "1 MB"},
		{int64(1.5 * 1024 * 1024 * 1024), "1.5 GB"},
	}
	for _, c := range cases {
		got := FormatBytes(c.n)
		if got != c.want {
			t.Errorf("FormatBytes(%d) = %q, want %q", c.n, got, c.want)
		}
	}
}

func TestFormatCount(t *testing.T) {
	cases := []struct {
		n    int64
		want string
	}{
		{0, "0"},
		{999, "999"},
		{1000, "1,000"},
		{1847, "1,847"},
		{1166, "1,166"},
		{1234567, "1,234,567"},
		{-4200, "-4,200"},
	}
	for _, c := range cases {
		got := FormatCount(c.n)
		if got != c.want {
			t.Errorf("FormatCount(%d) = %q, want %q", c.n, got, c.want)
		}
	}
}

func TestFormatDuration(t *testing.T) {
	cases := []struct {
		d    time.Duration
		want string
	}{
		{0, "0 sec"},
		{45 * time.Second, "45 sec"},
		{7*time.Minute + 12*time.Second, "7 min 12 sec"},
		{2*time.Hour + 3*time.Minute + 59*time.Second, "2 hr 3 min"},
		{59 * time.Second, "59 sec"},
		{60 * time.Second, "1 min 0 sec"},
	}
	for _, c := range cases {
		got := FormatDuration(c.d)
		if got != c.want {
			t.Errorf("FormatDuration(%v) = %q, want %q", c.d, got, c.want)
		}
	}
}

func TestRenderKnownETA(t *testing.T) {
	snap := Snapshot{
		BytesDone:  214 * 1024 * 1024,
		BytesTotal: 340 * 1024 * 1024,
		FilesDone:  1166,
		FilesTotal: 1847,
		ByteRate:   18.4 * 1024 * 1024,
		FileRate:   94,
		Percent:    62.94,
		Elapsed:    time.Minute,
		ETA:        7*time.Minute + 12*time.Second,
		ETAKnown:   true,
	}
	out := Render(snap, 24)
	if !strings.Contains(out, "63%") {
		t.Errorf("Render output missing rounded percent 63%%: %q", out)
	}
	if !strings.Contains(out, "214/340 MB") {
		t.Errorf("Render output missing byte pair: %q", out)
	}
	if !strings.Contains(out, "1,166/1,847 files") {
		t.Errorf("Render output missing file pair: %q", out)
	}
	if !strings.Contains(out, "18.4 MB/s") {
		t.Errorf("Render output missing byte rate: %q", out)
	}
	if !strings.Contains(out, "94 files/s") {
		t.Errorf("Render output missing file rate: %q", out)
	}
	if !strings.Contains(out, "7 min 12 sec remaining") {
		t.Errorf("Render output missing ETA: %q", out)
	}
	if strings.Contains(out, "estimating") {
		t.Errorf("Render output should not say estimating when ETAKnown: %q", out)
	}
}

func TestRenderUnknownETA(t *testing.T) {
	snap := Snapshot{
		BytesDone:  10,
		BytesTotal: 1000,
		FilesDone:  1,
		FilesTotal: 100,
		Percent:    1,
		Elapsed:    2 * time.Second,
		ETAKnown:   false,
	}
	out := Render(snap, 24)
	if !strings.Contains(out, "estimating…") {
		t.Errorf("Render output should say estimating… when ETA unknown: %q", out)
	}
	if strings.Contains(out, "remaining") {
		t.Errorf("Render output should not claim a remaining time when unknown: %q", out)
	}
}

func TestBarFillsProportionally(t *testing.T) {
	full := bar(100, 10)
	if strings.Contains(full, string(emptyRune)) {
		t.Errorf("bar(100, 10) should be fully filled, got %q", full)
	}
	empty := bar(0, 10)
	if strings.Contains(empty, string(filledRune)) {
		t.Errorf("bar(0, 10) should be fully empty, got %q", empty)
	}
	half := bar(50, 10)
	if strings.Count(half, string(filledRune)) != 5 {
		t.Errorf("bar(50, 10) = %q, want 5 filled runes", half)
	}
}
