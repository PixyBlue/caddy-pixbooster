package pixbooster

import (
	"bytes"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"

	"github.com/caddyserver/caddy/v2"
	"github.com/caddyserver/caddy/v2/caddyconfig/caddyfile"
	"github.com/caddyserver/caddy/v2/caddyconfig/httpcaddyfile"
	"github.com/caddyserver/caddy/v2/modules/caddyhttp"
	"go.uber.org/zap"
	"golang.org/x/net/html"
)

func init() {
	caddy.RegisterModule(Pixbooster{})
	httpcaddyfile.RegisterHandlerDirective("pixbooster", parseCaddyfile)
}

type Pixbooster struct {
	logger  *zap.Logger
	siteURL string
}

func (Pixbooster) CaddyModule() caddy.ModuleInfo {
	return caddy.ModuleInfo{
		ID:  "http.handlers.pixbooster",
		New: func() caddy.Module { return new(Pixbooster) },
	}
}

func (p *Pixbooster) Provision(ctx caddy.Context) error {
	p.logger = ctx.Logger(p)
	return nil
}

func (p Pixbooster) ServeHTTP(w http.ResponseWriter, r *http.Request, next caddyhttp.Handler) error {
	rec := httptest.NewRecorder()
	next.ServeHTTP(rec, r)

	body, err := io.ReadAll(rec.Body)
	if err != nil {
		return err
	}

	doc, err := html.Parse(bytes.NewReader(body))
	if err != nil {
		return err
	}

	p.siteURL = r.Proto + "://" + r.Host

	images := p.collectImagesToReplace(doc, []*html.Node{})
	for _, img := range images {
		p.replaceImgWithPicture(img)
	}
	p.addWebPSources(doc)

	html.Render(w, doc)

	return nil
}

func (p *Pixbooster) isSameSite(imageURL string) bool {
	originalHost, err := url.Parse(p.siteURL)
	if err != nil {
		return false
	}
	imageHost, err := url.Parse(imageURL)
	if err != nil {
		return false
	}
	return originalHost.Host == imageHost.Host
}

func (p *Pixbooster) collectImagesToReplace(n *html.Node, images []*html.Node) []*html.Node {
	if n.Type == html.ElementNode && n.Data == "img" && p.isSameSite(p.getSrc(n)) && !p.isImageInsidePicture(n) {
		images = append(images, n)
	}
	for c := n.FirstChild; c != nil; c = c.NextSibling {
		images = p.collectImagesToReplace(c, images)
	}
	return images
}

func (p *Pixbooster) isImageInsidePicture(img *html.Node) bool {
	for parent := img.Parent; parent != nil; parent = parent.Parent {
		if parent.Type == html.ElementNode && parent.Data == "picture" {
			return true
		}
	}
	return false
}

func (p *Pixbooster) addWebPSources(n *html.Node) {
	if n.Type == html.ElementNode && n.Data == "picture" {
		p.addWebPSourcesToPicture(n)
	}

	for c := n.FirstChild; c != nil; c = c.NextSibling {
		p.addWebPSources(c)
	}
}

func (p *Pixbooster) getSrc(n *html.Node) string {
	for _, attr := range n.Attr {
		if attr.Key == "src" {
			return attr.Val
		}
	}
	return ""
}

func (p *Pixbooster) replaceImgWithPicture(n *html.Node) {
	picture := &html.Node{
		Type: html.ElementNode,
		Data: "picture",
	}
	for _, attr := range n.Attr {
		if attr.Key != "src" && attr.Key != "alt" {
			picture.Attr = append(picture.Attr, attr)
		}
	}

	source := &html.Node{
		Type: html.ElementNode,
		Data: "source",
		Attr: []html.Attribute{
			{Key: "srcset", Val: p.getSrc(n) + ".pixbooster.webp"},
			{Key: "type", Val: "image/webp"},
		},
	}

	img := &html.Node{
		Type: html.ElementNode,
		Data: "img",
		Attr: n.Attr,
	}

	picture.AppendChild(source)
	picture.AppendChild(img)
	n.Parent.InsertBefore(picture, n)
	n.Parent.RemoveChild(n)
}

func (p *Pixbooster) addWebPSourcesToPicture(picture *html.Node) {
	var jpegSources []*html.Node

	for c := picture.FirstChild; c != nil; c = c.NextSibling {
		if c.Type == html.ElementNode && c.Data == "source" {
			if c.Attr[1].Val == "image/jpeg" || c.Attr[1].Val == "image/png" {
				jpegSources = append(jpegSources, c)
			}
		}
	}

	for _, source := range jpegSources {
		newSource := &html.Node{
			Type: html.ElementNode,
			Data: "source",
			Attr: make([]html.Attribute, 0, len(source.Attr)),
		}

		for _, attr := range source.Attr {
			if attr.Key != "srcset" && attr.Key != "type" {
				newSource.Attr = append(newSource.Attr, attr)
			}
		}

		filename := source.Attr[0].Val
		ext := ".webp"

		newSource.Attr = append(newSource.Attr, html.Attribute{
			Key: "srcset",
			Val: filename + ext,
		})

		newSource.Attr = append(newSource.Attr, html.Attribute{
			Key: "type",
			Val: "image/webp",
		})

		picture.InsertBefore(newSource, source)
		newSource.Parent = picture
	}
}

func (p *Pixbooster) UnmarshalCaddyfile(d *caddyfile.Dispenser) error {
	d.Next()
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
