package main

import (
	"fmt"
	"image"
	"image/color"
	"image/draw"
	"image/jpeg"
	"os"
)

// Contact sheets are composed here rather than by shelling out to ImageMagick.
// Installing that on the host failed with 404s across every mirror -- a stale
// package database, whose real fix is a full system upgrade -- and this keeps
// the bot's zero-dependency rule intact besides.
//
// The standard library has no resampler, so downscaling is a box filter written
// out below: for each destination pixel, average the source pixels it covers.
// That is a handful of lines, and for thumbnails it is indistinguishable from
// anything cleverer.

var backgroundGrey = color.RGBA{0x1c, 0x1c, 0x1c, 0xff}
var placeholderGrey = color.RGBA{0x2a, 0x2a, 0x2a, 0xff}
var placeholderInk = color.RGBA{0x70, 0x70, 0x70, 0xff}

// A 5x7 bitmap font covering the placeholder caption and nothing else. The
// standard library cannot draw text and the bot is built with GOPROXY=off, so
// the alternative to seven hand-drawn glyphs is a module dependency.
var glyphs = map[rune][7]string{
	'N': {"X   X", "XX  X", "X X X", "X  XX", "X   X", "X   X", "X   X"},
	'O': {" XXX ", "X   X", "X   X", "X   X", "X   X", "X   X", " XXX "},
	'S': {" XXXX", "X    ", "X    ", " XXX ", "    X", "    X", "XXXX "},
	'T': {"XXXXX", "  X  ", "  X  ", "  X  ", "  X  ", "  X  ", "  X  "},
	'I': {"XXXXX", "  X  ", "  X  ", "  X  ", "  X  ", "  X  ", "XXXXX"},
	'L': {"X    ", "X    ", "X    ", "X    ", "X    ", "X    ", "XXXXX"},
	' ': {"     ", "     ", "     ", "     ", "     ", "     ", "     "},
}

const (
	glyphW     = 5
	glyphH     = 7
	glyphScale = 3
	glyphGap   = 1
)

// drawText writes s in the bitmap font with its top-left at x, y.
func drawText(dst *image.RGBA, s string, x, y int, c color.Color) {
	for _, r := range s {
		g, ok := glyphs[r]
		if !ok {
			x += (glyphW + glyphGap) * glyphScale
			continue
		}
		for row := 0; row < glyphH; row++ {
			for col := 0; col < glyphW; col++ {
				if g[row][col] != 'X' {
					continue
				}
				px := x + col*glyphScale
				py := y + row*glyphScale
				draw.Draw(dst, image.Rect(px, py, px+glyphScale, py+glyphScale),
					&image.Uniform{c}, image.Point{}, draw.Src)
			}
		}
		x += (glyphW + glyphGap) * glyphScale
	}
}

// placeholderTile stands in for a clip that has no still, keeping the tile
// count equal to the clip count so the buttons stay aligned.
func placeholderTile(w, h int) *image.RGBA {
	t := image.NewRGBA(image.Rect(0, 0, w, h))
	draw.Draw(t, t.Bounds(), &image.Uniform{placeholderGrey}, image.Point{}, draw.Src)

	const label = "NO STILL"
	textW := len(label)*(glyphW+glyphGap)*glyphScale - glyphGap*glyphScale
	drawText(t, label, (w-textW)/2, (h-glyphH*glyphScale)/2, placeholderInk)
	return t
}

const (
	tileWidth  = 260
	tileCols   = 4
	tilePad    = 6
	sheetJPEGQ = 85
)

// boxScale downsamples src to exactly w by h.
func boxScale(src image.Image, w, h int) *image.RGBA {
	dst := image.NewRGBA(image.Rect(0, 0, w, h))
	sb := src.Bounds()
	for y := 0; y < h; y++ {
		y0 := sb.Min.Y + y*sb.Dy()/h
		y1 := sb.Min.Y + (y+1)*sb.Dy()/h
		if y1 <= y0 {
			y1 = y0 + 1
		}
		for x := 0; x < w; x++ {
			x0 := sb.Min.X + x*sb.Dx()/w
			x1 := sb.Min.X + (x+1)*sb.Dx()/w
			if x1 <= x0 {
				x1 = x0 + 1
			}
			var r, g, b, n uint64
			for sy := y0; sy < y1; sy++ {
				for sx := x0; sx < x1; sx++ {
					cr, cg, cb, _ := src.At(sx, sy).RGBA()
					r += uint64(cr >> 8)
					g += uint64(cg >> 8)
					b += uint64(cb >> 8)
					n++
				}
			}
			if n == 0 {
				continue
			}
			i := dst.PixOffset(x, y)
			dst.Pix[i+0] = uint8(r / n)
			dst.Pix[i+1] = uint8(g / n)
			dst.Pix[i+2] = uint8(b / n)
			dst.Pix[i+3] = 0xff
		}
	}
	return dst
}

// buildSheet tiles the given images into one JPEG and returns how many of them
// carried a usable picture.
//
// Every path gets a tile, including an empty path and one that fails to decode:
// those become a placeholder. The count of tiles has to equal the count of
// clips because the buttons beneath the sheet are laid out four to a row to sit
// under the tile they play, so a skipped tile shifts every button after it onto
// the wrong picture.
func buildSheet(paths []string, dest string) (int, error) {
	tiles := make([]*image.RGBA, len(paths))
	placed := 0
	for i, p := range paths {
		if p == "" {
			continue
		}
		f, err := os.Open(p)
		if err != nil {
			continue
		}
		img, err := jpeg.Decode(f)
		f.Close()
		if err != nil {
			continue
		}
		b := img.Bounds()
		if b.Dx() == 0 || b.Dy() == 0 {
			continue
		}
		h := tileWidth * b.Dy() / b.Dx()
		if h < 1 {
			h = 1
		}
		tiles[i] = boxScale(img, tileWidth, h)
		placed++
	}
	if len(tiles) == 0 {
		return 0, nil
	}

	// Placeholders take the shape of the pictures they sit beside, so a row
	// keeps one height. With nothing to copy, 16:9 matches the camera.
	placeholderHeight := tileWidth * 9 / 16
	for _, t := range tiles {
		if t != nil {
			placeholderHeight = t.Bounds().Dy()
			break
		}
	}
	for i, t := range tiles {
		if t == nil {
			tiles[i] = placeholderTile(tileWidth, placeholderHeight)
		}
	}

	// Rows are as tall as their tallest tile, so a portrait still among
	// landscape ones does not overlap its neighbours.
	rows := (len(tiles) + tileCols - 1) / tileCols
	rowHeight := make([]int, rows)
	for i, t := range tiles {
		r := i / tileCols
		if hh := t.Bounds().Dy(); hh > rowHeight[r] {
			rowHeight[r] = hh
		}
	}

	cols := tileCols
	if len(tiles) < cols {
		cols = len(tiles)
	}
	width := cols*tileWidth + (cols+1)*tilePad
	height := tilePad
	for _, h := range rowHeight {
		height += h + tilePad
	}

	sheet := image.NewRGBA(image.Rect(0, 0, width, height))
	draw.Draw(sheet, sheet.Bounds(), &image.Uniform{backgroundGrey}, image.Point{}, draw.Src)

	y := tilePad
	for r := 0; r < rows; r++ {
		x := tilePad
		for c := 0; c < tileCols; c++ {
			i := r*tileCols + c
			if i >= len(tiles) {
				break
			}
			t := tiles[i]
			draw.Draw(sheet, image.Rect(x, y, x+t.Bounds().Dx(), y+t.Bounds().Dy()),
				t, image.Point{}, draw.Src)
			x += tileWidth + tilePad
		}
		y += rowHeight[r] + tilePad
	}

	out, err := os.Create(dest)
	if err != nil {
		return 0, err
	}
	defer out.Close()
	if err := jpeg.Encode(out, sheet, &jpeg.Options{Quality: sheetJPEGQ}); err != nil {
		return 0, fmt.Errorf("encoding sheet: %w", err)
	}
	return placed, nil
}
