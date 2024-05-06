//go:build cgo

package pixbooster

import (
	"bytes"
	"fmt"
	"image"
	"image/jpeg"
	"image/png"
	"io"
	"net/http"

	"github.com/caddyserver/caddy/v2"
	"github.com/chai2010/webp"
	"github.com/gen2brain/avif"
	"github.com/gen2brain/jpegxl"
)

func (p *Pixbooster) Provision(ctx caddy.Context) error {
	p.CGOEnabled = true
	p.logger = ctx.Logger(p)
	p.imgSuffix = "pixbooster"
	p.destFormats = append(p.destFormats, imgFormat{extension: ".jxl", mimeType: "image/jxl"})
	p.destFormats = append(p.destFormats, imgFormat{extension: ".avif", mimeType: "image/avif"})
	p.destFormats = append(p.destFormats, imgFormat{extension: ".webp", mimeType: "image/webp"})
	p.srcFormats = append(p.srcFormats, imgFormat{extension: ".jpg", mimeType: "image/jpeg"})
	p.srcFormats = append(p.srcFormats, imgFormat{extension: ".png", mimeType: "image/png"})
	p.srcFormats = append(p.srcFormats, imgFormat{extension: ".webp", mimeType: "image/webp"})

	return nil
}

func (p *Pixbooster) convertImageToFormat(imgURL string, format imgFormat) (io.Reader, error) {
	resp, err := http.Get(imgURL)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()

	contentType := resp.Header.Get("Content-Type")

	var img image.Image
	var decodeErr error

	switch contentType {
	case "image/jpeg":
		img, decodeErr = jpeg.Decode(resp.Body)
	case "image/png":
		img, decodeErr = png.Decode(resp.Body)
	case "image/webp":
		img, decodeErr = webp.Decode(resp.Body)
	default:
		return nil, fmt.Errorf("unsupported input image format: %s", format.extension)
	}
	if decodeErr != nil {
		return nil, decodeErr
	}

	buf := new(bytes.Buffer)

	switch format.extension {
	case ".webp":
		err = webp.Encode(buf, img, &webp.Options{Quality: float32(p.WebpConfig.Quality), Lossless: p.WebpConfig.Lossless, Exact: p.WebpConfig.Exact})
	case ".avif":
		err = avif.Encode(buf, img, p.AvifConfig)
	case ".jxl":
		err = jpegxl.Encode(buf, img, p.JxlConfig)
	default:
		return nil, fmt.Errorf("unsupported output image format: %s", format.extension)
	}

	if err != nil {
		return nil, err
	}

	return buf, nil
}

func (p *Pixbooster) configureCGO() {}
