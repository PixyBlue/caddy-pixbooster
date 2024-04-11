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
	Extension string
	MimeType  string
}

type Pixbooster struct {
	logger      *zap.Logger
	rootURL     string
	imgSuffix   string
	destFormats []ImgFormat
	srcMimes    []string
	nowebp      bool
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
	p.destFormats = append(p.destFormats, ImgFormat{Extension: ".jxl", MimeType: "image/jxl"})
	p.destFormats = append(p.destFormats, ImgFormat{Extension: ".avif", MimeType: "image/avif"})
	p.destFormats = append(p.destFormats, ImgFormat{Extension: ".webp", MimeType: "image/webp"})
	p.srcMimes = append(p.srcMimes, "image/jpeg")
	p.srcMimes = append(p.srcMimes, "image/png")

	return nil
}

func (p Pixbooster) ServeHTTP(w http.ResponseWriter, r *http.Request, next caddyhttp.Handler) error {
	p.rootURL = p.getRootUrl(r)
	if p.isOptimizedUrl(r.URL.Path) {
		format := ImgFormat{}
		for _, f := range p.destFormats {
			if strings.HasSuffix(r.URL.Path, f.Extension) {
				format = f
				break
			}
		}
		if format.Extension == "" {
			http.Error(w, "Unsupported image format", http.StatusBadRequest)
			p.logger.Error("Unsupported image format: " + r.URL.Path)
			return fmt.Errorf("Unsupported image format: " + r.URL.Path)
		}

		originalImageUrl := p.getOriginalImageURL(p.rootURL + r.RequestURI)
		imgStream, err := p.convertImageToFormat(originalImageUrl, format)
		if err != nil {
			p.logger.Error("Error converting image to format: " + format.Extension)
			p.logger.Sugar().Error(err)
			http.Error(w, "Internal server error", http.StatusInternalServerError)
			return nil
		}

		w.Header().Set("Content-Type", format.MimeType)

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

			pictures := p.collectPictures(doc, []*html.Node{})
			imgs := p.collectImg(doc, []*html.Node{})

			for _, img := range imgs {
				p.wrapImgWithPicture(img)
			}

			for _, picture := range pictures {
				p.addSourcesToPicture(picture, false)
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

func (p *Pixbooster) getRootUrl(r *http.Request) string {
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

func (p *Pixbooster) isSameSite(imageURL string) bool {
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

func (p *Pixbooster) collectImg(n *html.Node, images []*html.Node) []*html.Node {
	if n.Type == html.ElementNode && n.Data == "img" && p.isSameSite(p.getSrc(n)) && !p.isImageInsidePicture(n) {
		src := p.getSrc(n)
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
		images = p.collectImg(c, images)
	}
	return images
}

func (p *Pixbooster) collectPictures(n *html.Node, pictures []*html.Node) []*html.Node {
	if n.Type == html.ElementNode && n.Data == "picture" {
		pictures = append(pictures, n)
	}
	for c := n.FirstChild; c != nil; c = c.NextSibling {
		pictures = p.collectPictures(c, pictures)
	}
	return pictures
}

func (p *Pixbooster) isImageInsidePicture(img *html.Node) bool {
	for parent := img.Parent; parent != nil; parent = parent.Parent {
		if parent.Type == html.ElementNode && parent.Data == "picture" {
			return true
		}
	}
	return false
}

func (p *Pixbooster) getAttr(n *html.Node, attribute string) string {
	for _, attr := range n.Attr {
		if attr.Key == attribute {
			return attr.Val
		}
	}
	return ""
}

func (p *Pixbooster) getSrc(n *html.Node) string {
	return p.getAttr(n, "src")
}

func (p *Pixbooster) wrapImgWithPicture(n *html.Node) {
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
	p.addSourcesToPicture(picture, true)
}

func (p *Pixbooster) addSourcesToPicture(picture *html.Node, copyAttr bool) {
	var imgSources []*html.Node
	var existingSource *html.Node
	for c := picture.FirstChild; c != nil; c = c.NextSibling {
		if c.Type == html.ElementNode && (c.Data == "source" || (c.Data == "img" && !p.isImageInsidePicture(c))) {
			var src string
			if c.Data == "img" {
				src = p.getSrc(c)
			} else {
				src = p.getAttr(c, "srcset")
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
				src = p.getSrc(source)
			} else {
				src = p.getAttr(source, "srcset")
			}

			newSource.Attr = append(newSource.Attr, html.Attribute{
				Key: "srcset",
				Val: p.getOptimizedImageURL(src, format),
			})

			newSource.Attr = append(newSource.Attr, html.Attribute{
				Key: "type",
				Val: format.MimeType,
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

func (p *Pixbooster) getOptimizedImageURL(originalURL string, format ImgFormat) string {
	parsedURL, err := url.Parse(originalURL)
	if err != nil {
		p.logger.Sugar().Fatalf("Error parsing URL: %v", err)
	}

	newPath := parsedURL.Path + "." + p.imgSuffix + format.Extension

	parsedURL.Path = newPath

	return parsedURL.String()
}

func (p *Pixbooster) getOriginalImageURL(optimizedURL string) string {

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

func (p *Pixbooster) isOptimizedUrl(myurl string) bool {
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

func (p *Pixbooster) convertImageToFormat(imgURL string, format ImgFormat) (io.Reader, error) {
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
		return nil, fmt.Errorf("unsupported input image format: %s", format.Extension)
	}
	if decodeErr != nil {
		return nil, decodeErr
	}

	buf := new(bytes.Buffer)

	switch format.Extension {
	case ".webp":
		err = webp.Encode(buf, img, &webp.Options{Lossless: true})
	case ".avif":
		err = avif.Encode(buf, img)
	case ".jxl":
		err = jpegxl.Encode(buf, img)
	default:
		return nil, fmt.Errorf("unsupported output image format: %s", format.Extension)
	}

	if err != nil {
		return nil, err
	}

	return buf, nil
}

func (p *Pixbooster) UnmarshalCaddyfile(d *caddyfile.Dispenser) error {
	for d.Next() {
		if d.NextArg() {
			return d.ArgErr()
		}
		for d.NextBlock(0) {
			switch d.Val() {
			case "nowebp":
				p.nowebp = true
			default:
				return d.Errf("unrecognized subdirective: %s", d.Val())
			}
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
