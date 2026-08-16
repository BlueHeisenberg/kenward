//go:build ignore

// mkicon writes the application icon the platform bundles need.
//
//	go run packaging/mkicon.go
//
// It exists so that the two binary files beside it are reviewable: an icon nobody can
// regenerate is an icon nobody can change. macOS needs a .icns, which is built from
// the PNG at package time by packaging/macos/bundle.sh using sips and iconutil —
// tools every Mac already has, and the reason no .icns is committed here.
//
// The mark is a placeholder: a ring, in the same slate the tray icon's neutral state
// uses. It is deliberately not one of the three status colours, because the
// application icon is not a status.
package main

import (
	"bytes"
	"encoding/binary"
	"image"
	"image/color"
	"image/png"
	"log"
	"math"
	"os"
)

func main() {
	big := render(512)
	if err := os.WriteFile("packaging/kenward.png", big, 0o644); err != nil {
		log.Fatal(err)
	}
	// 256 is the largest size an ICONDIRENTRY can name without the 0-means-256
	// convention, and it is what Windows scales the shortcut icon from.
	if err := os.WriteFile("packaging/kenward.ico", ico(render(256), 256), 0o644); err != nil {
		log.Fatal(err)
	}
}

func render(size int) []byte {
	var (
		fg     = color.NRGBA{0xe8, 0xe4, 0xdc, 0xff}
		bg     = color.NRGBA{0x2b, 0x2a, 0x28, 0xff}
		s      = float64(size)
		centre = s / 2
		corner = s * 0.22 // a rounded square, the shape every platform expects
		outer  = s * 0.30
		inner  = s * 0.20
	)
	img := image.NewNRGBA(image.Rect(0, 0, size, size))
	for y := range size {
		for x := range size {
			px, py := float64(x)+0.5, float64(y)+0.5
			if !inRoundedSquare(px, py, s, corner) {
				continue
			}
			r := math.Hypot(px-centre, py-centre)
			c := bg
			if r <= outer && r >= inner {
				c = fg
			}
			img.SetNRGBA(x, y, c)
		}
	}
	var buf bytes.Buffer
	if err := png.Encode(&buf, img); err != nil {
		log.Fatal(err)
	}
	return buf.Bytes()
}

func inRoundedSquare(x, y, size, corner float64) bool {
	// Distance to the nearest inset rectangle, which is the standard way to test a
	// rounded rectangle without four special cases.
	dx := math.Max(math.Max(corner-x, x-(size-corner)), 0)
	dy := math.Max(math.Max(corner-y, y-(size-corner)), 0)
	return math.Hypot(dx, dy) <= corner
}

// ico is the same twenty-two byte container cmd/kenward-desktop builds for the tray
// icon: an ICONDIR, one ICONDIRENTRY, and the PNG unchanged.
func ico(pngBytes []byte, size int) []byte {
	const headerLen = 6 + 16
	out := make([]byte, headerLen, headerLen+len(pngBytes))
	binary.LittleEndian.PutUint16(out[2:], 1)
	binary.LittleEndian.PutUint16(out[4:], 1)
	out[6] = byte(size % 256) // 256 is written as zero
	out[7] = byte(size % 256)
	binary.LittleEndian.PutUint16(out[10:], 1)
	binary.LittleEndian.PutUint16(out[12:], 32)
	binary.LittleEndian.PutUint32(out[14:], uint32(len(pngBytes)))
	binary.LittleEndian.PutUint32(out[18:], headerLen)
	return append(out, pngBytes...)
}
