package main

import (
	"bytes"
	"image"
	"image/color"
	"image/png"
	"math"
)

// The tray icon is drawn here rather than shipped as three files in the repository.
//
// It is a status light: a disc thirty-two pixels across, in one of three colours, and
// generating it is about forty lines of image/png against three binary blobs nobody
// can review in a diff. There is no artwork to lose.
//
// The three states differ in shape as well as in colour. A tray full of coloured dots
// is unreadable to a colour-blind user and to anyone whose panel recolours icons to
// match the theme — which GNOME's tray extensions do — so running is a solid disc,
// stopped is a hollow ring, and failed is a disc struck through.

const iconSize = 32

// supersample is how many samples per axis the disc is drawn at before averaging.
// Four is enough that the edge does not look like a staircase at 16px, which is what
// Windows actually renders.
const supersample = 4

var (
	colourRunning = color.NRGBA{0x2f, 0xa8, 0x4f, 0xff}
	colourStopped = color.NRGBA{0x8c, 0x8c, 0x8c, 0xff}
	colourFailed  = color.NRGBA{0xc7, 0x40, 0x2f, 0xff}
)

// icons are built once at startup. SetIcon is called on every state change, and on
// Windows fyne.io/systray materialises the bytes to a temp file each time, so there
// is no reason to re-encode the same three images.
var icons = map[daemonState][]byte{
	stateRunning: packIcon(drawIcon(colourRunning, shapeDisc)),
	stateStopped: packIcon(drawIcon(colourStopped, shapeRing)),
	stateFailed:  packIcon(drawIcon(colourFailed, shapeStruck)),
}

func iconFor(st daemonState) []byte { return icons[st] }

type shape int

const (
	shapeDisc shape = iota
	shapeRing
	shapeStruck
)

func drawIcon(c color.NRGBA, s shape) []byte {
	const (
		centre = float64(iconSize) / 2
		outer  = float64(iconSize)/2 - 2 // a two-pixel margin so nothing is clipped
		inner  = outer - 5               // the hole in the ring, and the bar's half-height
	)
	img := image.NewNRGBA(image.Rect(0, 0, iconSize, iconSize))
	for y := range iconSize {
		for x := range iconSize {
			var covered int
			for sy := range supersample {
				for sx := range supersample {
					px := float64(x) + (float64(sx)+0.5)/supersample
					py := float64(y) + (float64(sy)+0.5)/supersample
					if inShape(px-centre, py-centre, outer, inner, s) {
						covered++
					}
				}
			}
			if covered == 0 {
				continue
			}
			alpha := uint8(covered * 255 / (supersample * supersample))
			// Straight alpha: NRGBA keeps the colour unmultiplied, so a partly
			// covered edge pixel is the same hue as the middle.
			img.SetNRGBA(x, y, color.NRGBA{c.R, c.G, c.B, alpha})
		}
	}
	var buf bytes.Buffer
	if err := png.Encode(&buf, img); err != nil {
		// image/png cannot fail on an in-memory NRGBA of a fixed size, and an icon
		// is not worth a startup refusal even if it somehow did.
		return nil
	}
	return buf.Bytes()
}

func inShape(dx, dy, outer, inner float64, s shape) bool {
	r := math.Hypot(dx, dy)
	if r > outer {
		return false
	}
	switch s {
	case shapeRing:
		return r >= inner
	case shapeStruck:
		// A bar across the middle, cut out of the disc, so the shape reads as
		// "wrong" rather than merely "a different colour".
		return math.Abs(dy) > 2.5
	default:
		return true
	}
}
