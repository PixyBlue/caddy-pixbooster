package pixbooster

import (
	"bytes"
	"fmt"
	"image"
	"image/jpeg"
	"image/png"
	"io"
	"mime"
	"net/http"
	"net/http/httptest"
	"net/url"
	"path/filepath"
	"strings"

	"github.com/caddyserver/caddy/v2"
	"github.com/caddyserver/caddy/v2/caddyconfig/caddyfile"
	"github.com/caddyserver/caddy/v2/caddyconfig/httpcaddyfile"
	"github.com/caddyserver/caddy/v2/modules/caddyhttp"
	"github.com/chai2010/webp"
	"github.com/gen2brain/avif"
	"github.com/gen2brain/jpegxl"
	"go.uber.org/zap"
	"golang.org/x/net/html"
)

func init() {
	caddy.RegisterModule(Pixbooster{})
	httpcaddyfile.RegisterHandlerDirective("pixbooster", parseCaddyfile)
}

type ImgFormat struct {
	extension string
	mimeType  string
}

type Pixbooster struct {
	logger      *zap.Logger
	rootURL     string
	imgSuffix   string
	destFormats []ImgFormat
	srcMimes    []string
	Nowebp      bool
	Noavif      bool
	Nojxl       bool
}

func (Pixbooster) CaddyModule() caddy.ModuleInfo {
	return caddy.ModuleInfo{
		ID:  "http.handlers.pixbooster",
		New: func() caddy.Module { return new(Pixbooster) },
	}
}

func (p *Pixbooster) Provision(ctx caddy.Context) error {
	p.logger = ctx.Logger(p)
	p.imgSuffix = "pixbooster"
	p.destFormats = append(p.destFormats, ImgFormat{extension: ".jxl", mimeType: "image/jxl"})
	p.destFormats = append(p.destFormats, ImgFormat{extension: ".avif", mimeType: "image/avif"})
	p.destFormats = append(p.destFormats, ImgFormat{extension: ".webp", mimeType: "image/webp"})
	p.srcMimes = append(p.srcMimes, "image/jpeg")
	p.srcMimes = append(p.srcMimes, "image/png")

	return nil
}

func (p Pixbooster) ServeHTTP(w http.ResponseWriter, r *http.Request, next caddyhttp.Handler) error {
	p.rootURL = p.GetRootUrl(r)
	if p.IsOptimizedUrl(r.URL.Path) {
		format := ImgFormat{}
		for _, f := range p.destFormats {
			if strings.HasSuffix(r.URL.Path, f.extension) {
				format = f
				break
			}
		}
		if format.extension == "" {
			http.Error(w, "Unsupported image format", http.StatusBadRequest)
			p.logger.Error("Unsupported image format: " + r.URL.Path)
			return fmt.Errorf("Unsupported image format: " + r.URL.Path)
		}
		if p.Nowebp && format.extension == ".webp" || p.Noavif && format.extension == ".avif" || p.Nojxl && format.extension == ".jxl" {
			p.logger.Error(format.extension + "file requested but disabled by configuration")
			http.Error(w, "Internal server error", http.StatusInternalServerError)
			return nil
		}

		originalImageUrl := p.GetOriginalImageURL(p.rootURL + r.RequestURI)
		imgStream, err := p.ConvertImageToFormat(originalImageUrl, format)
		if err != nil {
			p.logger.Error("Error converting image to format: " + format.extension)
			p.logger.Sugar().Error(err)
			http.Error(w, "Internal server error", http.StatusInternalServerError)
			return err
		}

		w.Header().Set("Content-Type", format.mimeType)

		if _, err := io.Copy(w, imgStream); err != nil {
			p.logger.Error("Error sending image data: " + err.Error())
			http.Error(w, "Internal server error", http.StatusInternalServerError)
			return nil
		}
	}

	if next != nil {
		rec := httptest.NewRecorder()
		err := next.ServeHTTP(rec, r)
		if err != nil {
			return err
		}

		contentType := rec.Header().Get("Content-Type")
		if strings.HasPrefix(contentType, "text/html") {

			body := rec.Body.Bytes()
			doc, err := html.Parse(bytes.NewReader(body))
			if err != nil {
				return err
			}

			pictures := p.CollectPictures(doc, []*html.Node{})
			imgs := p.CollectImg(doc, []*html.Node{})

			for _, img := range imgs {
				p.WrapImgWithPicture(img)
			}

			for _, picture := range pictures {
				p.AddSourcesToPicture(picture, false)
			}

			w.Header().Set("Content-Type", "text/html")
			if err := html.Render(w, doc); err != nil {
				return err
			}
			return nil
		}

		for k, v := range rec.Header() {
			w.Header()[k] = v
		}
		w.WriteHeader(rec.Code)
		_, err = io.Copy(w, rec.Body)
		return err
	}

	http.Error(w, "Not Found", http.StatusNotFound)
	return nil
}

func (p *Pixbooster) GetRootUrl(r *http.Request) string {
	var proto string
	if r.TLS == nil {
		proto = "http://"
	} else {
		proto = "https://"
	}
	var port string
	if r.URL.Port() != "" {
		port = ":" + r.URL.Port()
	}
	return proto + r.Host + port
}

func (p *Pixbooster) IsSameSite(imageURL string) bool {
	if !strings.HasPrefix(imageURL, "http://") && !strings.HasPrefix(imageURL, "https://") {
		return true
	}

	imageURLParsed, err := url.Parse(imageURL)
	if err != nil {
		p.logger.Sugar().Debug(err)
		return false
	}

	return imageURLParsed.Host == p.rootURL
}

func (p *Pixbooster) CollectImg(n *html.Node, images []*html.Node) []*html.Node {
	if n.Type == html.ElementNode && n.Data == "img" && p.IsSameSite(p.GetSrc(n)) && !p.IsImageInsidePicture(n) {
		src := p.GetSrc(n)
		ext := filepath.Ext(src)
		mimeType := mime.TypeByExtension(ext)
		for _, allowedMimeType := range p.srcMimes {
			if allowedMimeType == mimeType {
				images = append(images, n)
				break
			}
		}
	}

	for c := n.FirstChild; c != nil; c = c.NextSibling {
		images = p.CollectImg(c, images)
	}
	return images
}

func (p *Pixbooster) CollectPictures(n *html.Node, pictures []*html.Node) []*html.Node {
	if n.Type == html.ElementNode && n.Data == "picture" {
		pictures = append(pictures, n)
	}
	for c := n.FirstChild; c != nil; c = c.NextSibling {
		pictures = p.CollectPictures(c, pictures)
	}
	return pictures
}

func (p *Pixbooster) IsImageInsidePicture(img *html.Node) bool {
	for parent := img.Parent; parent != nil; parent = parent.Parent {
		if parent.Type == html.ElementNode && parent.Data == "picture" {
			return true
		}
	}
	return false
}

func (p *Pixbooster) GetAttr(n *html.Node, attribute string) string {
	for _, attr := range n.Attr {
		if attr.Key == attribute {
			return attr.Val
		}
	}
	return ""
}

func (p *Pixbooster) GetSrc(n *html.Node) string {
	return p.GetAttr(n, "src")
}

func (p *Pixbooster) WrapImgWithPicture(n *html.Node) {
	picture := &html.Node{
		Type: html.ElementNode,
		Data: "picture",
	}
	for _, attr := range n.Attr {
		if attr.Key != "src" && attr.Key != "alt" {
			picture.Attr = append(picture.Attr, attr)
		}
	}

	img := &html.Node{
		Type: html.ElementNode,
		Data: "img",
		Attr: n.Attr,
	}

	picture.AppendChild(img)
	n.Parent.InsertBefore(picture, n)
	n.Parent.RemoveChild(n)
	p.AddSourcesToPicture(picture, true)
}

func (p *Pixbooster) AddSourcesToPicture(picture *html.Node, copyAttr bool) {
	var imgSources []*html.Node
	var existingSource *html.Node
	for c := picture.FirstChild; c != nil; c = c.NextSibling {
		if c.Type == html.ElementNode && (c.Data == "source" || (c.Data == "img" && !p.IsImageInsidePicture(c))) {
			var src string
			if c.Data == "img" {
				src = p.GetSrc(c)
			} else {
				src = p.GetAttr(c, "srcset")
			}

			ext := filepath.Ext(src)
			mimeType := mime.TypeByExtension(ext)
			for _, allowedMimeType := range p.srcMimes {
				if mimeType == allowedMimeType {
					if c.Data == "source" && existingSource == nil {
						existingSource = c
					}
					imgSources = append(imgSources, c)
					break
				}
			}
		}
	}

	for _, source := range imgSources {
		for _, format := range p.destFormats {
			p.logger.Sugar().Debug(p.Noavif, p.Nojxl, p.Nowebp)
			if p.Nowebp && format.extension == ".webp" || p.Noavif && format.extension == ".avif" || p.Nojxl && format.extension == ".jxl" {
				continue
			}
			newSource := &html.Node{
				Type: html.ElementNode,
				Data: "source",
				Attr: make([]html.Attribute, 0, len(source.Attr)),
			}

			if copyAttr {
				for _, attr := range source.Attr {
					if attr.Key != "srcset" && attr.Key != "type" && attr.Key != "src" {
						newSource.Attr = append(newSource.Attr, attr)
					}
				}
			}

			var src string
			if source.Data == "img" {
				src = p.GetSrc(source)
			} else {
				src = p.GetAttr(source, "srcset")
			}

			newSource.Attr = append(newSource.Attr, html.Attribute{
				Key: "srcset",
				Val: p.GetOptimizedImageURL(src, format),
			})

			newSource.Attr = append(newSource.Attr, html.Attribute{
				Key: "type",
				Val: format.mimeType,
			})

			if existingSource != nil {
				picture.InsertBefore(newSource, existingSource)
			} else {
				picture.AppendChild(newSource)
			}
			newSource.Parent = picture
		}
	}
}

func (p *Pixbooster) GetOptimizedImageURL(originalURL string, format ImgFormat) string {
	parsedURL, err := url.Parse(originalURL)
	if err != nil {
		p.logger.Sugar().Fatalf("Error parsing URL: %v", err)
	}

	newPath := parsedURL.Path + "." + p.imgSuffix + format.extension

	parsedURL.Path = newPath

	return parsedURL.String()
}

func (p *Pixbooster) GetOriginalImageURL(optimizedURL string) string {

	pathParts := strings.Split(optimizedURL, ".")
	pixboosterIndex := -1

	for i, part := range pathParts {
		if part == p.imgSuffix {
			pixboosterIndex = i
			break
		}
	}

	if pixboosterIndex == -1 {
		p.logger.Sugar().Fatalf("Error finding %s suffix in URL: %s", p.imgSuffix, optimizedURL)
	}

	return strings.Join(pathParts[:pixboosterIndex], ".")
}

func (p *Pixbooster) IsOptimizedUrl(myurl string) bool {
	parsedURL, err := url.Parse(myurl)
	if err != nil {
		p.logger.Sugar().Errorf("Error parsing URL: %v", err)
		return false
	}

	pathParts := strings.Split(parsedURL.Path, ".")
	pixboosterIndex := -1

	for i, part := range pathParts {
		if part == p.imgSuffix {
			pixboosterIndex = i
			break
		}
	}

	return pixboosterIndex != -1
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
	case ".webp":
		err = webp.Encode(buf, img, &webp.Options{Lossless: true})
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

func (p *Pixbooster) UnmarshalCaddyfile(d *caddyfile.Dispenser) error {
	d.Next()
	for d.Next() {
		switch d.Val() {
		case "nowebp":
			p.Nowebp = true
		case "noavif":
			p.Noavif = true
		case "nojxl":
			p.Nojxl = true
		default:
			return d.ArgErr()
		}
	}

	return nil
}

func parseCaddyfile(h httpcaddyfile.Helper) (caddyhttp.MiddlewareHandler, error) {
	var p Pixbooster
	err := p.UnmarshalCaddyfile(h.Dispenser)
	return p, err
}

// Interface guards
var (
	_ caddy.Provisioner           = (*Pixbooster)(nil)
	_ caddyhttp.MiddlewareHandler = (*Pixbooster)(nil)
	_ caddyfile.Unmarshaler       = (*Pixbooster)(nil)
)
