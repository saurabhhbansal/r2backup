package tray

import (
	"bytes"
	_ "embed"
	"encoding/binary"
	"image"
	"image/color"
	"image/png"
	"math"
	"sync"
)

// The tray icon is the logo's stack -- the same mark as the panel's header --
// drawn from its embedded alpha mask in black or white, whichever reads on
// the taskbar, with a dot in the corner whose colour says how the backups are
// doing. One shape, so it is recognisable at 16 pixels; the dot is the part
// that changes. The app is monochrome: only the dot carries colour, and only
// when something needs saying.

//go:embed logo-mark.png
var logoMarkPNG []byte

var (
	// ink is the stack on a dark taskbar; paper is the same on a light one.
	ink   = color.NRGBA{0xf4, 0xf4, 0xf5, 0xff}
	paper = color.NRGBA{0x18, 0x18, 0x1b, 0xff}

	dotColours = map[string]color.NRGBA{
		KindOK:        {0x34, 0xd3, 0x99, 0xff},
		KindAttention: {0xf8, 0x71, 0x71, 0xff},
		KindPending:   {0xfb, 0xbf, 0x24, 0xff},
		KindSetup:     {0xfb, 0xbf, 0x24, 0xff},
	}
)

// Icon returns a Windows .ico for a status kind, for a dark taskbar, at the
// sizes the notification area asks for.
func Icon(kind string) []byte { return IconFor(kind, false) }

// IconFor is Icon for either taskbar: lightTaskbar draws the stack dark so it
// does not vanish on a light one.
func IconFor(kind string, lightTaskbar bool) []byte {
	fg := ink
	if lightTaskbar {
		fg = paper
	}
	sizes := []int{16, 20, 24, 32, 48}
	var pngs [][]byte
	for _, s := range sizes {
		var buf bytes.Buffer
		_ = png.Encode(&buf, drawIcon(s, kind, fg, nil))
		pngs = append(pngs, buf.Bytes())
	}
	return packICO(sizes, pngs)
}

// AppIcon is the programs' own icon -- the one Explorer, the Start menu and
// Task Manager show for r2b.exe and r2bw.exe: the mark alone, with no status
// dot, light on a dark rounded tile so it reads on any background, up to the
// 256 pixels Explorer's largest view asks for. cmd/r2b's winres_gen.go builds
// it into the binaries.
func AppIcon() []byte {
	sizes := []int{16, 20, 24, 32, 40, 48, 64, 256}
	tile := paper
	var pngs [][]byte
	for _, s := range sizes {
		var buf bytes.Buffer
		_ = png.Encode(&buf, drawIcon(s, "", ink, &tile))
		pngs = append(pngs, buf.Bytes())
	}
	return packICO(sizes, pngs)
}

// stackMask is the logo's alpha channel: one byte per pixel, row by row.
type stackMask struct {
	a    []uint8
	w, h int
}

var (
	maskOnce sync.Once
	mask     stackMask
)

func loadMask() stackMask {
	maskOnce.Do(func() {
		img, err := png.Decode(bytes.NewReader(logoMarkPNG))
		if err != nil {
			panic("tray: embedded logo-mark.png: " + err.Error())
		}
		b := img.Bounds()
		mask = stackMask{a: make([]uint8, b.Dx()*b.Dy()), w: b.Dx(), h: b.Dy()}
		for y := 0; y < mask.h; y++ {
			for x := 0; x < mask.w; x++ {
				_, _, _, al := img.At(b.Min.X+x, b.Min.Y+y).RGBA()
				mask.a[y*mask.w+x] = uint8(al >> 8)
			}
		}
	})
	return mask
}

// at returns the logo's coverage, 0..255, at a point given as fractions of
// its width and height; outside the logo there is none.
func (m stackMask) at(fx, fy float64) int {
	if fx < 0 || fy < 0 || fx >= 1 || fy >= 1 {
		return 0
	}
	return int(m.a[int(fy*float64(m.h))*m.w+int(fx*float64(m.w))])
}

// drawIcon renders one size, with supersampled anti-aliasing so the edges
// are not jagged at 16 pixels. A non-nil tile puts the stack on a rounded
// square of that colour, smaller, with margin.
func drawIcon(size int, kind string, fg color.NRGBA, tile *color.NRGBA) *image.NRGBA {
	img := image.NewNRGBA(image.Rect(0, 0, size, size))
	s := float64(size)
	m := loadMask()
	mh := s * 0.94 // the stack's height, nearly the whole square
	if tile != nil {
		mh = s * 0.6
	}
	mw := mh * float64(m.w) / float64(m.h)
	mx, my := (s-mw)/2, (s-mh)/2
	dot, hasDot := dotColours[kind]
	if kind == KindRunning || kind == KindInterrupted {
		dot, hasDot = fg, true
	}
	dr := s * 0.2 // dot radius
	dx, dy := s-dr-0.5, s-dr-0.5
	cr := s * 0.22 // the tile's corner radius
	const ss = 8   // supersamples per axis
	for y := 0; y < size; y++ {
		for x := 0; x < size; x++ {
			var mark, inDot, ring, covered, inTile int
			for sy := 0; sy < ss; sy++ {
				for sx := 0; sx < ss; sx++ {
					px := float64(x) + (float64(sx)+0.5)/ss
					py := float64(y) + (float64(sy)+0.5)/ss
					if a := m.at((px-mx)/mw, (py-my)/mh); a > 0 {
						mark++
						covered += a
					}
					if tile != nil {
						// distance outside the rounded square's inner box
						ox := math.Max(math.Max(cr-px, px-(s-cr)), 0)
						oy := math.Max(math.Max(cr-py, py-(s-cr)), 0)
						if math.Hypot(ox, oy) <= cr {
							inTile++
						}
					}
					if hasDot {
						d := math.Hypot(px-dx, py-dy)
						if d <= dr {
							inDot++
						} else if d <= dr+math.Max(1, s/16) {
							ring++ // a clear gap between dot and mark
						}
					}
				}
			}
			n := ss * ss
			switch {
			case inDot > 0:
				img.SetNRGBA(x, y, blend(dot, inDot, n))
			case ring > 0 && mark > 0:
				// the gap: transparent, so the dot reads separately
			case tile != nil && inTile > 0:
				bg := blend(*tile, inTile, n)
				if mark > 0 {
					// the stack over the tile, by its own coverage
					f := blend(fg, covered, n*255)
					img.SetNRGBA(x, y, over(f, bg))
				} else {
					img.SetNRGBA(x, y, bg)
				}
			case mark > 0 && tile == nil:
				img.SetNRGBA(x, y, blend(fg, covered, n*255))
			}
		}
	}
	return img
}

// over composites a over b, both straight (non-premultiplied) alpha.
func over(a, b color.NRGBA) color.NRGBA {
	af, bf := float64(a.A)/255, float64(b.A)/255
	oa := af + bf*(1-af)
	if oa == 0 {
		return color.NRGBA{}
	}
	mix := func(ac, bc uint8) uint8 {
		return uint8((float64(ac)*af + float64(bc)*bf*(1-af)) / oa)
	}
	return color.NRGBA{mix(a.R, b.R), mix(a.G, b.G), mix(a.B, b.B), uint8(oa * 255)}
}

func blend(c color.NRGBA, cover, of int) color.NRGBA {
	c.A = uint8(int(c.A) * cover / of)
	return c
}

// packICO wraps PNG images in an ICO container, which Windows Vista and
// later read directly.
func packICO(sizes []int, pngs [][]byte) []byte {
	var b bytes.Buffer
	_ = binary.Write(&b, binary.LittleEndian, [3]uint16{0, 1, uint16(len(pngs))})
	offset := 6 + 16*len(pngs)
	for i, p := range pngs {
		dim := byte(sizes[i])
		if sizes[i] >= 256 {
			dim = 0
		}
		b.Write([]byte{dim, dim, 0, 0})
		_ = binary.Write(&b, binary.LittleEndian, uint16(1))  // planes
		_ = binary.Write(&b, binary.LittleEndian, uint16(32)) // bits per pixel
		_ = binary.Write(&b, binary.LittleEndian, uint32(len(p)))
		_ = binary.Write(&b, binary.LittleEndian, uint32(offset))
		offset += len(p)
	}
	for _, p := range pngs {
		b.Write(p)
	}
	return b.Bytes()
}
