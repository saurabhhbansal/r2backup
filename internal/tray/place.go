package tray

// Where the panel opens. The arithmetic lives here, apart from the Windows
// calls that feed it, so it can be tested on any platform.

type rect struct{ Left, Top, Right, Bottom int32 }

func (r rect) empty() bool { return r.Right <= r.Left || r.Bottom <= r.Top }

// The screen edge a taskbar is docked to, numbered as Windows numbers them
// (ABE_LEFT and so on).
const (
	edgeLeft   = 0
	edgeTop    = 1
	edgeRight  = 2
	edgeBottom = 3
)

// edgeOf works out which edge a taskbar is on from the monitor and the part
// of it left over for windows: the taskbar is wherever the two differ most.
// It is the fallback for when Windows will not say.
func edgeOf(monitor, work rect) int {
	gaps := [4]int32{
		edgeLeft:   work.Left - monitor.Left,
		edgeTop:    work.Top - monitor.Top,
		edgeRight:  monitor.Right - work.Right,
		edgeBottom: monitor.Bottom - work.Bottom,
	}
	best := edgeBottom
	for e, g := range gaps {
		if g > gaps[best] {
			best = e
		}
	}
	return best
}

// place returns the top-left corner for a w by h panel: inside work (the
// monitor less its taskbar), against the taskbar's edge, and beside the
// icon when its rectangle is known. Without one it takes the corner the
// notification area sits in -- the right end of a horizontal taskbar, the
// bottom of a vertical one -- which is where Windows opens its own flyouts.
// gap is the margin kept from every edge.
func place(work rect, edge int, icon rect, w, h, gap int32) (x, y int32) {
	// Along the taskbar: centred on the icon, or in the far corner.
	x = work.Right - w - gap
	y = work.Bottom - h - gap
	if !icon.empty() {
		x = (icon.Left+icon.Right)/2 - w/2
		y = (icon.Top+icon.Bottom)/2 - h/2
	}
	// Against the taskbar.
	switch edge {
	case edgeTop:
		y = work.Top + gap
	case edgeLeft:
		x = work.Left + gap
	case edgeRight:
		x = work.Right - w - gap
	default:
		y = work.Bottom - h - gap
	}
	// Never off the monitor: a panel bigger than the space still starts at
	// its top-left, so the header and its buttons stay reachable.
	x = clamp(x, work.Left+gap, work.Right-w-gap)
	y = clamp(y, work.Top+gap, work.Bottom-h-gap)
	return x, y
}

func clamp(v, lo, hi int32) int32 {
	if v > hi {
		v = hi
	}
	if v < lo {
		v = lo
	}
	return v
}
