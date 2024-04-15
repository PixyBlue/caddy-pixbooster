//go:build !cgo

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
	"github.com/gen2brain/avif"
	"github.com/gen2brain/jpegxl"
)

func (p *Pixbooster) Provision(ctx caddy.Context) error {
	p.CGOEnabled = false
	p.logger = ctx.Logger(p)
	p.imgSuffix = "pixbooster"
	p.destFormats = append(p.destFormats, ImgFormat{extension: ".jxl", mimeType: "image/jxl"})
	p.destFormats = append(p.destFormats, ImgFormat{extension: ".avif", mimeType: "image/avif"})
	p.srcFormats = append(p.srcFormats, ImgFormat{extension: ".jpg", mimeType: "image/jpeg"})
	p.srcFormats = append(p.srcFormats, ImgFormat{extension: ".png", mimeType: "image/png"})

	return nil
}

func (p *Pixbooster) ConvertImageToFormat(imgURL string, format ImgFormat) (io.Reader, error) {
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
	default:
		return nil, fmt.Errorf("unsupported input image format: %s", format.extension)
	}
	if decodeErr != nil {
		return nil, decodeErr
	}

	buf := new(bytes.Buffer)

	switch format.extension {
	case ".avif":
		err = avif.Encode(buf, img)
	case ".jxl":
		err = jpegxl.Encode(buf, img)
	default:
		return nil, fmt.Errorf("unsupported output image format: %s", format.extension)
	}

	if err != nil {
		return nil, err
	}

	return buf, nil
}

func (p *Pixbooster) ConfigureCGO() {
	p.Nowebpoutput = false
	p.Nowebpinput = false

}
