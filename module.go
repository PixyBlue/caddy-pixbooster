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
	p.srcFormats = append(p.srcFormats, ImgFormat{extension: ".jpg", mimeType: "image/jpeg"})
	p.srcFormats = append(p.srcFormats, ImgFormat{extension: ".png", mimeType: "image/png"})
	p.srcFormats = append(p.srcFormats, ImgFormat{extension: ".webp", mimeType: "image/webp"})

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
			imgs := p.CollectImgs(doc, []*html.Node{})

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
//	pixbooster [nowebpoutput|noavif|nojxl|nojpg|nopng]
func (p *Pixbooster) UnmarshalCaddyfile(d *caddyfile.Dispenser) error {
	d.Next()
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
