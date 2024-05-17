package pixbooster

import (
	"bytes"
	"crypto/md5"
	"encoding/hex"
	"fmt"
	"io"
	"mime"
	"net/http"
	"net/url"
	"os"
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

type imgFormat struct {
	extension string
	mimeType  string
}

// Pixbooster allows your server to provide pictures in modern file formats (webp, avif, jxl) on the fly without requiring you to change your html.
type Pixbooster struct {
	cGOEnabled  bool
	logger      *zap.Logger
	rootURL     string
	imgSuffix   string
	destFormats []imgFormat
	srcFormats  []imgFormat
	// Path where to store the modern image files. Optional.
	Storage string `json:"storage,omitempty"`
	// Disable Webp output if present.
	Nowebpoutput bool `json:"nowebpoutput,omitempty"`
	// Disable treatment of Webp files in the incomming HTML if present.
	Nowebpinput bool `json:"nowebpinput,omitempty"`
	// Disable Avif output  if present.
	Noavif bool `json:"noavif,omitempty"`
	// Disable JXL output if present.
	Nojxl bool `json:"nojxl,omitempty"`
	// Disable treatment of JPEG files in the incomming HTML if present.
	Nojpeg bool `json:"nojpeg,omitempty"`
	// Disable treatment of PNG files in the incomming HTML if present.
	Nopng bool `json:"nopng,omitempty"`

	// Quality of output pictures, a integer between 0 and 100. Optional.
	Quality int `json:"quality,omitempty"`
	// Set specific Webp ouput options.
	WebpConfig WebpConfig `json:"webp_config,omitempty"`
	// Set specific Avif ouput options.
	AvifConfig avif.Options `json:"avif_config,omitempty"`
	// Set specific JXL ouput options.
	JxlConfig jpegxl.Options `json:"jxl_config,omitempty"`
}

type WebpConfig struct {
	// Quality of output pictures, a integer between 0 and 100. Optional.
	Quality int `json:"quality,omitempty"`
	// Enable lossless quality compression if present.
	Lossless bool `json:"lossless,omitempty"`
	Exact    bool `json:"exact,omitempty"`
}

func (Pixbooster) CaddyModule() caddy.ModuleInfo {
	return caddy.ModuleInfo{
		ID:  "http.handlers.pixbooster",
		New: func() caddy.Module { return new(Pixbooster) },
	}
}

func (p Pixbooster) ServeHTTP(w http.ResponseWriter, r *http.Request, next caddyhttp.Handler) error {
	p.logger.Debug("Pixbooster start")
	p.rootURL = p.getRootUrl(r)
	if p.isOptimizedUrl(r.URL.Path) {
		optimizedFileName := filepath.Join(p.Storage, p.getOptimizedFileName(r.URL.Path))
		if data, err := os.ReadFile(optimizedFileName); err == nil {
			w.Write(data)
			return nil
		} else if !os.IsNotExist(err) {
			p.logger.Error("Unable to access Pixbooster storage")
			http.Error(w, "Internal Server Error", http.StatusInternalServerError)
			return err
		}

		p.logger.Debug("Optimized image URL: " + r.URL.Path)
		format := imgFormat{}
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

		originalImageUrl := p.getOriginalImageURL(p.rootURL + r.RequestURI)
		p.logger.Debug("Original image URL: " + originalImageUrl)
		imgStream, err := p.convertImageToFormat(originalImageUrl, format)
		if err != nil {
			p.logger.Error("Error converting image to format: " + format.extension)
			p.logger.Sugar().Error(err)
			http.Error(w, "Internal server error", http.StatusInternalServerError)
			return err
		}

		w.Header().Set("Content-Type", format.mimeType)

		data, err := io.ReadAll(imgStream)
		if err != nil {
			p.logger.Error("Error reading image data: " + err.Error())
			http.Error(w, "Internal server error", http.StatusInternalServerError)
			return nil
		}

		if _, err := w.Write(data); err != nil {
			p.logger.Error("Error sending image data: " + err.Error())
			http.Error(w, "Internal server error", http.StatusInternalServerError)
			return nil
		}

		file, err := os.Create(optimizedFileName)
		if err != nil {
			return err
		}
		defer file.Close()

		_, err = file.Write(data)
		return err
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

			pictures := p.collectPictures(doc, []*html.Node{})
			imgs := p.collectImgs(doc, []*html.Node{})

			for _, img := range imgs {
				p.wrapImgWithPicture(img)
			}

			for _, picture := range pictures {
				p.addSourcesToPicture(picture)
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

func (p *Pixbooster) collectImgs(n *html.Node, imgs []*html.Node) []*html.Node {
	if n.Type == html.ElementNode && n.Data == "img" && p.isSameSite(p.getAttr(n, "src")) && !p.hasAttr(n, "data-pixbooster-ignore") && !p.isImageInsidePicture(n) {
		src := p.getAttr(n, "src")
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
		imgs = p.collectImgs(c, imgs)
	}
	return imgs
}

func (p *Pixbooster) collectPictures(n *html.Node, pictures []*html.Node) []*html.Node {
	if n.Type == html.ElementNode && n.Data == "picture" && !p.hasAttr(n, "data-pixbooster-ignore") {
		pictures = append(pictures, n)
	}
	for c := n.FirstChild; c != nil; c = c.NextSibling {
		pictures = p.collectPictures(c, pictures)
	}
	return pictures
}

func (p *Pixbooster) hasAttr(n *html.Node, name string) bool {
	for _, attr := range n.Attr {
		if attr.Key == name {
			return true
		}
	}
	return false
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

func (p *Pixbooster) wrapImgWithPicture(n *html.Node) {
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
	p.addSourcesToPicture(picture)
}

func (p *Pixbooster) addSourcesToPicture(picture *html.Node) {
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
		p.addSourcesToSource(source)
	}
}

func (p *Pixbooster) addSourcesToSource(source *html.Node) {
	if p.hasAttr(source, "srcset") {
		for _, format := range p.destFormats {
			if p.isOutputFormatAllowed(format) {
				p.addSourceNode(source, p.getOptimizedSrcset(p.getAttr(source, "srcset"), format), format.mimeType, source.Data == "source")
			}
		}
	}

	src := p.getAttr(source, "src")
	if source.Data == "img" && src != "" && p.isSameSite(src) && p.isInputFormatAllowed(src) {
		for _, format := range p.destFormats {
			if p.isOutputFormatAllowed(format) {
				p.addSourceNode(source, p.getOptimizedImageURL(src, format), format.mimeType, false)
			}
		}
	}
}

func (p *Pixbooster) getOptimizedSrcset(srcset string, format imgFormat) string {
	srcsetParts := strings.Split(srcset, ",")

	for i, part := range srcsetParts {
		part = strings.TrimSpace(part)
		subParts := strings.Fields(part)

		for j, subPart := range subParts {
			if p.isInputFormatAllowed(subPart) && p.isSameSite(subPart) {
				subParts[j] = p.getOptimizedImageURL(subPart, format)
			}
		}

		srcsetParts[i] = strings.Join(subParts, " ")
	}

	return strings.Join(srcsetParts, ",")
}

func (p *Pixbooster) addSourceNode(n *html.Node, srcset string, mimeType string, copyAttr bool) {
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

func (p *Pixbooster) getOptimizedImageURL(originalURL string, format imgFormat) string {
	parsedURL, err := url.Parse(originalURL)
	if err != nil {
		p.logger.Sugar().Fatalf("Error parsing URL: %v", err)
	}

	newPath := parsedURL.Path + "." + p.imgSuffix + format.extension

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

func (p *Pixbooster) isOutputFormatAllowed(format imgFormat) bool {
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
	var format imgFormat
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

func (p *Pixbooster) getOptimizedFileName(originalURL string) string {
	hash := md5.Sum([]byte(originalURL))
	return hex.EncodeToString(hash[:])
}

// UnmarshalCaddyfile implements caddyfile.Unmarshaler. Syntax:
//
//	pixbooster [nowebpoutput|noavif|nojxl|nojpg|nopng] {
//		[nowebpoutput|noavif|nojxl|nojpg|nopng]
//		quality <integer between 0 and 100>
//		storage <directory> Path to the directory where to store generated picture files
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
	p.Storage = caddy.AppConfigDir() + "/pixbooster"
	_, err := os.Stat(p.Storage)
	if os.IsNotExist(err) {
		err := os.MkdirAll(p.Storage, 0755)
		if err != nil {
			p.logger.Sugar().Warn("Error creating default storage directory:", err)
		}
	}

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
		case "storage":
			if !d.NextArg() {
				return d.ArgErr()
			}
			storage := d.Val()
			f, err := os.OpenFile(filepath.Join(storage, "test_write_file"), os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0644)
			if err == nil {
				p.Storage = storage
				defer os.Remove(f.Name())
			} else {
				p.logger.Error("Configured storage unusable, fallback to default")
				p.logger.Sugar().Error(err)
			}
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

	p.configureCGO()

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
