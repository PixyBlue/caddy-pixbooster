package pixbooster

import (
	"bytes"
	"fmt"
	"io"
	"mime"
	"net/http"
	"net/url"
	"path/filepath"
	"strconv"
	"strings"

	"github.com/caddyserver/caddy/v2"
	"github.com/caddyserver/caddy/v2/caddyconfig/caddyfile"
	"github.com/caddyserver/caddy/v2/caddyconfig/httpcaddyfile"
	"github.com/caddyserver/caddy/v2/modules/caddyhttp"
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
	CGOEnabled   bool
	logger       *zap.Logger
	rootURL      string
	imgSuffix    string
	destFormats  []ImgFormat
	srcFormats   []ImgFormat
	Nowebpoutput bool
	Nowebpinput  bool
	Noavif       bool
	Nojxl        bool
	Nojpeg       bool
	Nopng        bool

	Quality    int
	WebpConfig WebpConfig
	AvifConfig avif.Options
	JxlConfig  jpegxl.Options
}

type WebpConfig struct {
	Quality  int
	Lossless bool
	Exact    bool
}

func (Pixbooster) CaddyModule() caddy.ModuleInfo {
	return caddy.ModuleInfo{
		ID:  "http.handlers.pixbooster",
		New: func() caddy.Module { return new(Pixbooster) },
	}
}

func (p Pixbooster) ServeHTTP(w http.ResponseWriter, r *http.Request, next caddyhttp.Handler) error {
	p.logger.Debug("Pixbooster start")
	p.rootURL = p.GetRootUrl(r)
	if p.IsOptimizedUrl(r.URL.Path) {
		p.logger.Sugar().Debug(r.URL.Path)
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
		if !p.isOutputFormatAllowed(format) {
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
		buf := &bytes.Buffer{}
		rec := caddyhttp.NewResponseRecorder(w, buf, func(s int, h http.Header) bool { return true })
		err := next.ServeHTTP(rec, r)
		if err != nil {
			return err
		}

		contentType := rec.Header().Get("Content-Type")
		if strings.HasPrefix(contentType, "text/html") {

			body := buf.Bytes()
			doc, err := html.Parse(bytes.NewReader(body))
			if err != nil {
				return err
			}

			pictures := p.CollectPictures(doc, []*html.Node{})
			imgs := p.CollectImgs(doc, []*html.Node{})

			for _, img := range imgs {
				p.WrapImgWithPicture(img)
			}

			for _, picture := range pictures {
				p.AddSourcesToPicture(picture, false)
			}

			var result bytes.Buffer
			if err := html.Render(&result, doc); err != nil {
				return err
			}

			for k, vv := range rec.Header() {
				v := make([]string, len(vv))
				copy(v, vv)
				w.Header()[k] = v
			}
			delete(rec.Header(), "Content-Length")
			w.WriteHeader(rec.Status())
			_, err = io.Copy(w, &result)
			return err
		}

		for k, vv := range rec.Header() {
			v := make([]string, len(vv))
			copy(v, vv)
			w.Header()[k] = v
		}
		w.WriteHeader(rec.Status())
		_, err = io.Copy(w, buf)
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

func (p *Pixbooster) CollectImgs(n *html.Node, imgs []*html.Node) []*html.Node {
	if n.Type == html.ElementNode && n.Data == "img" && p.IsSameSite(p.GetAttr(n, "src")) && !p.HasAttr(n, "data-pixbooster-ignore") && !p.IsImageInsidePicture(n) {
		src := p.GetAttr(n, "src")
		ext := filepath.Ext(src)
		mimeType := mime.TypeByExtension(ext)
		p.logger.Debug(mimeType)
		for _, format := range p.srcFormats {
			if format.mimeType == mimeType {
				imgs = append(imgs, n)
				break
			}
		}
	}

	for c := n.FirstChild; c != nil; c = c.NextSibling {
		imgs = p.CollectImgs(c, imgs)
	}
	return imgs
}

func (p *Pixbooster) CollectPictures(n *html.Node, pictures []*html.Node) []*html.Node {
	if n.Type == html.ElementNode && n.Data == "picture" && !p.HasAttr(n, "data-pixbooster-ignore") {
		pictures = append(pictures, n)
	}
	for c := n.FirstChild; c != nil; c = c.NextSibling {
		pictures = p.CollectPictures(c, pictures)
	}
	return pictures
}

func (p *Pixbooster) HasAttr(n *html.Node, name string) bool {
	for _, attr := range n.Attr {
		if attr.Key == name {
			return true
		}
	}
	return false
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

func (p *Pixbooster) WrapImgWithPicture(n *html.Node) {
	picture := &html.Node{
		Type: html.ElementNode,
		Data: "picture",
	}
	for _, attr := range n.Attr {
		if attr.Key != "src" && attr.Key != "alt" && attr.Key != "srcset" {
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
	p.AddSourcesToPicture(picture, false)
}

func (p *Pixbooster) AddSourcesToPicture(picture *html.Node, copyAttr bool) {
	if picture.Data != "picture" {
		return
	}

	var sources []*html.Node
	var imgNode *html.Node

	for c := picture.FirstChild; c != nil; c = c.NextSibling {
		if c.Type == html.ElementNode {
			if c.Data == "source" {
				sources = append(sources, c)
			} else if c.Data == "img" && imgNode == nil {
				imgNode = c
			}
		}
	}

	if len(sources) == 0 && imgNode != nil {
		sources = append(sources, imgNode)
	}

	for _, source := range sources {
		p.AddSourcesToSource(source, copyAttr)
	}
}

func (p *Pixbooster) AddSourcesToSource(source *html.Node, copyAttr bool) {
	if p.HasAttr(source, "srcset") {
		for _, format := range p.destFormats {
			if p.isOutputFormatAllowed(format) {
				p.AddSourceNode(source, p.GetOptimizedSrcset(p.GetAttr(source, "srcset"), format), format.mimeType, source.Data == "source")
			}
		}
	}

	src := p.GetAttr(source, "src")
	if source.Data == "img" && src != "" && p.IsSameSite(src) && p.isInputFormatAllowed(src) {
		for _, format := range p.destFormats {
			if p.isOutputFormatAllowed(format) {
				p.AddSourceNode(source, p.GetOptimizedImageURL(src, format), format.mimeType, false)
			}
		}
	}
}

func (p *Pixbooster) GetOptimizedSrcset(srcset string, format ImgFormat) string {
	srcsetParts := strings.Split(srcset, ",")

	for i, part := range srcsetParts {
		part = strings.TrimSpace(part)
		subParts := strings.Fields(part)

		for j, subPart := range subParts {
			if p.isInputFormatAllowed(subPart) && p.IsSameSite(subPart) {
				subParts[j] = p.GetOptimizedImageURL(subPart, format)
			}
		}

		srcsetParts[i] = strings.Join(subParts, " ")
	}

	return strings.Join(srcsetParts, ",")
}

func (p *Pixbooster) AddSourceNode(n *html.Node, srcset string, mimeType string, copyAttr bool) {
	newSource := &html.Node{
		Type: html.ElementNode,
		Data: "source",
		Attr: make([]html.Attribute, 0, len(n.Attr)),
	}

	if copyAttr {
		for _, attr := range n.Attr {
			if attr.Key != "srcset" && attr.Key != "type" && attr.Key != "src" {
				newSource.Attr = append(newSource.Attr, attr)
			}
		}
	}

	newSource.Attr = append(newSource.Attr, html.Attribute{
		Key: "srcset",
		Val: srcset,
	})

	newSource.Attr = append(newSource.Attr, html.Attribute{
		Key: "type",
		Val: mimeType,
	})

	n.Parent.InsertBefore(newSource, n)
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

func (p *Pixbooster) isOutputFormatAllowed(format ImgFormat) bool {
	switch format.extension {
	case ".webp":
		return !p.Nowebpoutput
	case ".avif":
		return !p.Noavif
	case ".jxl":
		return !p.Nojxl
	default:
		return false
	}
}

func (p *Pixbooster) isInputFormatAllowed(filename string) bool {
	var format ImgFormat
	mimeType := mime.TypeByExtension(filepath.Ext(filename))
	for _, f := range p.srcFormats {
		if f.mimeType == mimeType {
			format = f
		}
	}

	switch format.extension {
	case ".jpg":
		return !p.Nojpeg
	case ".png":
		return !p.Nopng
	case ".webp":
		return !p.Nowebpinput
	default:
		return false
	}
}

// UnmarshalCaddyfile implements caddyfile.Unmarshaler. Syntax:
//
//	pixbooster [nowebpoutput|noavif|nojxl|nojpg|nopng] {
//		[nowebpoutput|noavif|nojxl|nojpg|nopng]
//		quality <integer between 0 and 100>
//		webp {
//			quality <integer between 0 and 100>
//			lossless
//			exact
//		}
//		avif {
//			quality <integer between 0 and 100>
//			qualityalpha <integer between 0 and 100>
//			speed <integer between 0 and 10>
//		}
//		jxl {
//			quality <integer between 0 and 100>
//			effort <integer between 0 and 10>
//		}
//	}
//
// The 'quality' value is inherited by webp.quality, avif.quality, and jxl.quality if not specified.
// The 'speed' and 'effort' values should be integers between 0 and 10.
// The 'lossless' and 'exact' flags are set to true if specified.
// All directives are optional.
func (p *Pixbooster) UnmarshalCaddyfile(d *caddyfile.Dispenser) error {
	var inBlock bool
	if d.NextBlock(0) {
		inBlock = true
	}

	for d.Next() {
		switch d.Val() {
		case "nowebpoutput":
			p.Nowebpoutput = true
		case "nowebpinput":
			p.Nowebpinput = true
		case "noavif":
			p.Noavif = true
		case "nojxl":
			p.Nojxl = true
		case "nojpeg":
			p.Nojpeg = true
		case "nopng":
			p.Nopng = true
		case "quality":
			if !d.NextArg() {
				return d.ArgErr()
			}
			quality, err := strconv.Atoi(d.Val())
			if err != nil || quality < 0 || quality > 100 {
				return fmt.Errorf("invalid quality value: %s", d.Val())
			}
			p.Quality = quality
		case "avif":
			if inBlock && d.NextBlock(0) {
				p.AvifConfig = avif.Options{Quality: p.Quality}
				for d.Next() {
					switch d.Val() {
					case "quality":
						if !d.NextArg() {
							return d.ArgErr()
						}
						quality, err := strconv.Atoi(d.Val())
						if err != nil || quality < 0 || quality > 100 {
							return fmt.Errorf("invalid avif quality value: %s", d.Val())
						}
						p.AvifConfig.Quality = quality
					case "qualityalpha":
						if !d.NextArg() {
							return d.ArgErr()
						}
						qualityAlpha, err := strconv.Atoi(d.Val())
						if err != nil || qualityAlpha < 0 || qualityAlpha > 100 {
							return fmt.Errorf("invalid avif qualityalpha value: %s", d.Val())
						}
						p.AvifConfig.QualityAlpha = qualityAlpha
					case "speed":
						if !d.NextArg() {
							return d.ArgErr()
						}
						speed, err := strconv.Atoi(d.Val())
						if err != nil || speed < 0 || speed > 10 {
							return fmt.Errorf("invalid avif speed value: %s", d.Val())
						}
						p.AvifConfig.Speed = speed
					default:
						return d.ArgErr()
					}
				}
			} else {
				p.AvifConfig = avif.Options{Quality: p.Quality}
			}
		case "jxl":
			if inBlock && d.NextBlock(0) {
				p.JxlConfig = jpegxl.Options{Quality: p.Quality}
				for d.Next() {
					switch d.Val() {
					case "quality":
						if !d.NextArg() {
							return d.ArgErr()
						}
						quality, err := strconv.Atoi(d.Val())
						if err != nil || quality < 0 || quality > 100 {
							return fmt.Errorf("invalid jxl quality value: %s", d.Val())
						}
						p.JxlConfig.Quality = quality
					case "effort":
						if !d.NextArg() {
							return d.ArgErr()
						}
						effort, err := strconv.Atoi(d.Val())
						if err != nil || effort < 0 || effort > 10 {
							return fmt.Errorf("invalid jxl effort value: %s", d.Val())
						}
						p.JxlConfig.Effort = effort
					default:
						return d.ArgErr()
					}
				}
			} else {
				return d.ArgErr()
			}
		case "webp":
			if inBlock && d.NextBlock(0) {
				p.WebpConfig = WebpConfig{Quality: p.Quality}
				for d.Next() {
					switch d.Val() {
					case "quality":
						if !d.NextArg() {
							return d.ArgErr()
						}
						quality, err := strconv.Atoi(d.Val())
						if err != nil || quality < 0 || quality > 100 {
							return fmt.Errorf("invalid webp quality value: %s", d.Val())
						}
						p.WebpConfig.Quality = quality
					case "lossless":
						p.WebpConfig.Lossless = true
					case "exact":
						p.WebpConfig.Exact = true
					default:
						return d.ArgErr()
					}
				}
			} else {
				return d.ArgErr()
			}
		default:
			if inBlock {
				return d.ArgErr()
			}
		}
	}

	p.ConfigureCGO()

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
