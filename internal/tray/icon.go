package tray

import (
	"bytes"
	"encoding/binary"
	"image"
	"image/color"
	"image/png"
	"math"
)

// The tray icon is drawn here rather than shipped as a file: a diamond --
// the same mark as the panel's header -- with a dot in the corner whose
// colour says how the backups are doing. One shape, so it is recognisable at
// 16 pixels; the dot is the part that changes.

var (
	markColour = color.NRGBA{0x38, 0xbd, 0xf8, 0xff}
	dotColours = map[string]color.NRGBA{
		KindOK:          {0x34, 0xd3, 0x99, 0xff},
		KindRunning:     {0x38, 0xbd, 0xf8, 0xff},
		KindInterrupted: {0x38, 0xbd, 0xf8, 0xff},
		KindAttention:   {0xf8, 0x71, 0x71, 0xff},
		KindPending:     {0xfb, 0xbf, 0x24, 0xff},
		KindSetup:       {0xfb, 0xbf, 0x24, 0xff},
	}
)

// Icon returns a Windows .ico for a status kind, at the sizes the
// notification area asks for.
func Icon(kind string) []byte {
	sizes := []int{16, 20, 24, 32, 48}
	var pngs [][]byte
	for _, s := range sizes {
		var buf bytes.Buffer
		_ = png.Encode(&buf, drawIcon(s, kind))
		pngs = append(pngs, buf.Bytes())
	}
	return packICO(sizes, pngs)
}

// AppIcon is the programs' own icon -- the one Explorer, the Start menu and
// Task Manager show for r2b.exe and r2bw.exe: the mark alone, with no status
// dot, up to the 256 pixels Explorer's largest view asks for. cmd/r2b's
// winres_gen.go builds it into the binaries.
func AppIcon() []byte {
	sizes := []int{16, 20, 24, 32, 40, 48, 64, 256}
	var pngs [][]byte
	for _, s := range sizes {
		var buf bytes.Buffer
		_ = png.Encode(&buf, drawIcon(s, ""))
		pngs = append(pngs, buf.Bytes())
	}
	return packICO(sizes, pngs)
}

// drawIcon renders one size, with simple coverage-based anti-aliasing so the
// diagonal edges are not jagged at 16 pixels.
func drawIcon(size int, kind string) *image.NRGBA {
	img := image.NewNRGBA(image.Rect(0, 0, size, size))
	s := float64(size)
	c := s / 2
	r := s * 0.46 // diamond half-diagonal
	dot, hasDot := dotColours[kind]
	dr := s * 0.2 // dot radius
	dx, dy := s-dr-0.5, s-dr-0.5
	const ss = 4 // supersamples per axis
	for y := 0; y < size; y++ {
		for x := 0; x < size; x++ {
			var mark, inDot, ring int
			for sy := 0; sy < ss; sy++ {
				for sx := 0; sx < ss; sx++ {
					px := float64(x) + (float64(sx)+0.5)/ss
					py := float64(y) + (float64(sy)+0.5)/ss
					if math.Abs(px-c)+math.Abs(py-c) <= r {
						mark++
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
			case mark > 0:
				img.SetNRGBA(x, y, blend(markColour, mark, n))
			}
		}
	}
	return img
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
