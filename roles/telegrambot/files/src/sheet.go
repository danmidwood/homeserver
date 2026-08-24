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

// buildSheet tiles the given images into one JPEG and returns how many it
// managed to place. Images that fail to decode are skipped rather than aborting
// the sheet: a single corrupt still should not cost the whole day's view.
func buildSheet(paths []string, dest string) (int, error) {
	var tiles []*image.RGBA
	for _, p := range paths {
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
		tiles = append(tiles, boxScale(img, tileWidth, h))
	}
	if len(tiles) == 0 {
		return 0, nil
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
	return len(tiles), nil
}
