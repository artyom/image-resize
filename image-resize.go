// Command image-resize resizes (re-scales) images of different formats
package main

import (
	"bytes"
	"errors"
	"flag"
	"fmt"
	"image"
	"image/jpeg"
	"image/png"
	"io"
	"io/ioutil"
	"os"
	"path/filepath"
	"strings"

	"github.com/artyom/image-resize/internal/gif"

	"github.com/artyom/autoflags"
	"github.com/bamiaux/rez"
	"github.com/disintegration/gift"
	"github.com/rwcarlsen/goexif/exif"
	"github.com/soniakeys/quant/mean"
	"golang.org/x/image/bmp"
	"golang.org/x/image/draw"
	"golang.org/x/image/tiff"
)

func main() {
	p := params{
		JpegQuality: jpeg.DefaultQuality,
	}
	autoflags.Define(&p)
	flag.Parse()
	if err := do(p); err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
}

type params struct {
	Width     int    `flag:"width,width to enforce"`
	Height    int    `flag:"height,height to enforce"`
	MaxWidth  int    `flag:"maxwidth,max. allowed width"`
	MaxHeight int    `flag:"maxheight,max. allowed height"`
	Input     string `flag:"input,input file"`
	Output    string `flag:"output,output file"`
	Square    bool   `flag:"square,crop image to square by smaller side before processing"`
	NoFill    bool   `flag:"nofill,do not draw transparent inputs over white for non-png outputs"`

	JpegQuality int `flag:"q,jpeg quality (1-100)"`
}

func do(par params) error {
	if par.JpegQuality < 1 || par.JpegQuality > 100 {
		par.JpegQuality = jpeg.DefaultQuality
	}
	tr, err := newTransform(par.Width, par.Height, par.MaxWidth, par.MaxHeight)
	if err != nil {
		return err
	}
	f, err := os.Open(par.Input)
	if err != nil {
		return err
	}
	defer f.Close()

	headBuf := new(bytes.Buffer)
	teeReader := io.TeeReader(f, headBuf)
	cfg, kind, err := image.DecodeConfig(teeReader)
	if err != nil {
		return err
	}
	if cfg.Width*cfg.Height > pixelLimit {
		return fmt.Errorf("image dimensions %dx%d exceeds limit", cfg.Width, cfg.Height)
	}
	width, height, err := tr.newDimensions(cfg.Width, cfg.Height)
	if err != nil {
		return err
	}

	imageDataReader := io.LimitReader(io.MultiReader(headBuf, f), maxFileSize)
	exifChan := make(chan exifData, 1)
	if kind == "jpeg" {
		prd, pwr := io.Pipe()
		defer pwr.Close()
		imageDataReader = io.TeeReader(imageDataReader, pwr)
		go func() {
			defer func() {
				if p := recover(); p != nil {
					fmt.Fprintln(os.Stderr, "exif decode failed")
				}
				io.Copy(ioutil.Discard, prd)
			}()
			data, err := exif.Decode(prd)
			exifChan <- exifData{data, err}
		}()
	}

	img, _, err := image.Decode(imageDataReader)
	if err != nil {
		return err
	}

	var rotatefunc func(image.Image) image.Image
	var swapWH bool
	if kind == "jpeg" {
		select {
		case ed := <-exifChan:
			rotatefunc, swapWH = useExifOrientation(ed)
		default:
			fmt.Fprintln(os.Stderr, "exif decode failed/stuck")
		}
	}
	if swapWH {
		par.Width, par.Height = par.Height, par.Width
		par.MaxWidth, par.MaxHeight = par.MaxHeight, par.MaxWidth
		tr, err = newTransform(par.Width, par.Height, par.MaxWidth, par.MaxHeight)
		if err != nil {
			return err
		}
		width, height, err = tr.newDimensions(cfg.Width, cfg.Height)
		if err != nil {
			return err
		}
	}
	if par.Square {
		type subImager interface {
			SubImage(r image.Rectangle) image.Image
		}
		si, ok := img.(subImager)
		if !ok {
			return errors.New("cannot crop image")
		}
		minSide := cfg.Width
		if cfg.Height < minSide {
			minSide = cfg.Height
		}
		x0, y0 := (cfg.Width-minSide)/2, (cfg.Height-minSide)/2
		img = si.SubImage(image.Rect(x0, y0, x0+minSide, y0+minSide))
		width, height, err = tr.newDimensions(minSide, minSide)
		if err != nil {
			return err
		}
	}
	outSuffix := strings.ToLower(filepath.Ext(par.Output))
	var outImg image.Image
	if (cfg.Width <= width && cfg.Height <= height) && (tr.MaxWidth > 0 || tr.MaxHeight > 0) {
		// noupscale case
		outImg = img
		goto saveOutput
	}
	switch img.(type) {
	case *image.YCbCr, *image.RGBA, *image.NRGBA, *image.Gray:
		outImg, err = resize(img, width, height, rez.NewLanczosFilter(3))
	default:
		outImg, err = resizeFallback(img, width, height)
	}
	if err != nil {
		return err
	}
saveOutput:
	if op, ok := outImg.(opaquer); ok && !par.NoFill && !op.Opaque() && outSuffix != ".png" {
		newOut := image.NewRGBA(outImg.Bounds())
		draw.Copy(newOut, image.Point{}, image.White, newOut.Bounds(), draw.Src, nil)
		draw.Copy(newOut, image.Point{}, outImg, newOut.Bounds(), draw.Over, nil)
		outImg = newOut
	}
	if rotatefunc != nil {
		outImg = rotatefunc(outImg)
	}
	of, err := os.Create(par.Output)
	if err != nil {
		return err
	}
	defer of.Close()
	switch outSuffix {
	case ".gif":
		gifOpts := &gif.Options{NumColors: 256, Quantizer: mean.Quantizer(256)}
		if pImg, ok := img.(*image.Paletted); ok {
			gifOpts.NumColors = len(pImg.Palette)
			gifOpts.Quantizer = mean.Quantizer(gifOpts.NumColors)
		}
		err = gif.Encode(of, outImg, gifOpts)
	case ".png":
		enc := png.Encoder{CompressionLevel: png.BestCompression}
		err = enc.Encode(of, outImg)
	case ".tiff", ".tif":
		err = tiff.Encode(of, outImg,
			&tiff.Options{Compression: tiff.Deflate, Predictor: true})
	case ".bmp":
		err = bmp.Encode(of, outImg)
	default:
		err = jpeg.Encode(of, outImg, &jpeg.Options{par.JpegQuality})
	}
	if err != nil {
		return err
	}
	return of.Close()
}

type transform struct {
	Width     int
	Height    int
	MaxWidth  int
	MaxHeight int
}

func (tr transform) newDimensions(origWidth, origHeight int) (width, height int, err error) {
	if origWidth == 0 || origHeight == 0 {
		return 0, 0, errors.New("invalid source dimensions")
	}
	var w, h int
	switch {
	case tr.MaxWidth > 0 || tr.MaxHeight > 0:
		w, h = tr.MaxWidth, tr.MaxHeight
		// if only one max dimension specified, calculate another using
		// original aspect ratio
		if w == 0 {
			w = origWidth * h / origHeight
		}
		if h == 0 {
			h = origHeight * w / origWidth
		}
		if origWidth <= w && origHeight <= h {
			return origWidth, origHeight, nil // image already fit
		}
		if tr.MaxWidth > 0 && tr.MaxHeight > 0 {
			// maxwidth and maxheight form free aspect ratio, need
			// to adjust w and h to match origin aspect ratio, while
			// keeping dimensions inside max bounds
			if float64(origWidth)/float64(origHeight) > float64(w)/float64(h) {
				h = origHeight * w / origWidth
			} else {
				w = origWidth * h / origHeight
			}
		}
	case tr.Width > 0 || tr.Height > 0:
		// if both width and height specified, free aspect ratio is
		// applied; if only one is set, original aspect ratio is kept
		w, h = tr.Width, tr.Height
		if w == 0 {
			w = origWidth * h / origHeight
		}
		if h == 0 {
			h = origHeight * w / origWidth
		}
	default:
		return 0, 0, fmt.Errorf("invalid transform %v", tr)
	}
	if w*h > pixelLimit || w >= 1<<16 || h >= 1<<16 {
		return 0, 0, errors.New("destination size exceeds limit")
	}
	return w, h, nil
}

func newTransform(width, height, maxWidth, maxHeight int) (transform, error) {
	tr := transform{
		Width:     width,
		Height:    height,
		MaxWidth:  maxWidth,
		MaxHeight: maxHeight,
	}
	if tr.Width == 0 && tr.Height == 0 && tr.MaxWidth == 0 && tr.MaxHeight == 0 {
		return transform{}, errors.New("no valid dimensions specified")
	}
	if tr.Width*tr.Height > pixelLimit || tr.MaxWidth > pixelLimit || tr.MaxHeight > pixelLimit {
		return transform{}, errors.New("destination size exceeds limit")
	}
	return tr, nil
}

func resize(inImg image.Image, width, height int, algo rez.Filter) (image.Image, error) {
	var outImg image.Image
	rect := image.Rect(0, 0, width, height)
	switch inImg.(type) {
	case *image.Gray:
		outImg = image.NewGray(rect)
	case *image.RGBA:
		outImg = image.NewRGBA(rect)
	case *image.NRGBA:
		outImg = image.NewNRGBA(rect)
	default:
		outImg = image.NewYCbCr(rect, image.YCbCrSubsampleRatio420)
	}
	if err := rez.Convert(outImg, inImg, algo); err != nil {
		return nil, err
	}
	return outImg, nil
}

func resizeFallback(inImg image.Image, width, height int) (image.Image, error) {
	outImg := image.NewRGBA(image.Rect(0, 0, width, height))
	draw.CatmullRom.Scale(outImg, outImg.Bounds(), inImg, inImg.Bounds(), draw.Src, nil)
	return outImg, nil
}

const (
	pixelLimit  = 50 * 1000000
	maxFileSize = 50 << 20
)

type opaquer interface {
	Opaque() bool
}

type exifData struct {
	exif *exif.Exif
	err  error
}

func useExifOrientation(ed exifData) (rotatefunc func(image.Image) image.Image, swapWH bool) {
	if ed.err != nil || ed.exif == nil {
		return
	}
	o, err := ed.exif.Get(exif.Orientation)
	if err != nil || o == nil || len(o.Val) != 2 {
		return
	}
	for _, x := range o.Val {
		switch x {
		case 3: // 180º
			return rotate180, false
		case 6: // 90ºCCW
			return rotate90ccw, true
		case 8: // 90ºCW
			return rotate90cw, true
		}
	}
	return
}

func rotate90ccw(src image.Image) image.Image { return rotate(src, gift.Rotate270()) }
func rotate90cw(src image.Image) image.Image  { return rotate(src, gift.Rotate90()) }
func rotate180(src image.Image) image.Image   { return rotate(src, gift.Rotate180()) }

func rotate(src image.Image, filter gift.Filter) image.Image {
	g := gift.New(filter)
	var dst draw.Image
	switch src.(type) {
	case *image.Gray:
		dst = image.NewGray(g.Bounds(src.Bounds()))
	default:
		dst = image.NewRGBA(g.Bounds(src.Bounds()))
	}
	g.Draw(dst, src)
	return dst
}
