package pixbooster

import (
	"net/http"

	"github.com/caddyserver/caddy/v2"
	"github.com/caddyserver/caddy/v2/modules/caddyhttp"
	"go.uber.org/zap"
	"github.com/caddyserver/caddy/v2/caddyconfig/caddyfile"
	"github.com/caddyserver/caddy/v2/caddyconfig/httpcaddyfile"
)

func init() {
	caddy.RegisterModule(Pixbooster{})
	httpcaddyfile.RegisterHandlerDirective("pixbooster", parseCaddyfile)
}

type Pixbooster struct {
	logger *zap.Logger
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
	p.logger.Info("Pixbooster middleware is working")
	return next.ServeHTTP(w, r)
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
