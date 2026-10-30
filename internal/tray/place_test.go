package tray

import "testing"

func TestPanelOpensBesideTheTray(t *testing.T) {
	const w, h, gap = 380, 580, 12
	none := rect{}
	// Work areas of a 1920x1080 monitor with a 48-pixel taskbar on each edge
	// in turn.
	bottom := rect{0, 0, 1920, 1032}
	top := rect{0, 48, 1920, 1080}
	left := rect{48, 0, 1920, 1080}
	right := rect{0, 0, 1872, 1080}

	cases := []struct {
		name         string
		work         rect
		edge         int
		icon         rect
		wantX, wantY int32
	}{
		{"bottom, no icon: bottom-right corner", bottom, edgeBottom, none, 1920 - w - gap, 1032 - h - gap},
		{"top, no icon: top-right corner", top, edgeTop, none, 1920 - w - gap, 48 + gap},
		{"left, no icon: bottom-left corner", left, edgeLeft, none, 48 + gap, 1080 - h - gap},
		{"right, no icon: bottom-right corner", right, edgeRight, none, 1872 - w - gap, 1080 - h - gap},
		{"bottom, icon mid-screen: centred on it", bottom, edgeBottom, rect{1000, 1040, 1024, 1072}, 1012 - w/2, 1032 - h - gap},
		{"bottom, icon at the far right: kept on screen", bottom, edgeBottom, rect{1880, 1040, 1904, 1072}, 1920 - w - gap, 1032 - h - gap},
		{"left, icon near the top: kept on screen", left, edgeLeft, rect{8, 20, 40, 44}, 48 + gap, gap},
		{"right, icon mid-height: centred on it", right, edgeRight, rect{1880, 500, 1904, 524}, 1872 - w - gap, 512 - h/2},
		{"a monitor left of the primary, at negative x", rect{-2560, 0, 0, 1344}, edgeBottom, none, -w - gap, 1344 - h - gap},
		{"the same, with the icon known", rect{-2560, 0, 0, 1344}, edgeBottom, rect{-100, 1350, -76, 1380}, -w - gap, 1344 - h - gap},
		{"work area smaller than the panel: top-left", rect{0, 0, 300, 400}, edgeBottom, none, gap, gap},
	}
	for _, c := range cases {
		x, y := place(c.work, c.edge, c.icon, w, h, gap)
		if x != c.wantX || y != c.wantY {
			t.Errorf("%s: got (%d, %d), want (%d, %d)", c.name, x, y, c.wantX, c.wantY)
		}
	}
}

func TestTaskbarEdgeFromWorkArea(t *testing.T) {
	mon := rect{0, 0, 1920, 1080}
	for _, c := range []struct {
		work rect
		want int
	}{
		{rect{0, 0, 1920, 1032}, edgeBottom},
		{rect{0, 48, 1920, 1080}, edgeTop},
		{rect{62, 0, 1920, 1080}, edgeLeft},
		{rect{0, 0, 1858, 1080}, edgeRight},
		{mon, edgeBottom}, // auto-hidden or absent: the usual place
	} {
		if got := edgeOf(mon, c.work); got != c.want {
			t.Errorf("edgeOf(%v) = %d, want %d", c.work, got, c.want)
		}
	}
}
