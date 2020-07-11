// Copyright 2015 Matthew Holt and The Caddy Authors
//
// Licensed under the Apache License, Version 2.0 (the "License");
// you may not use this file except in compliance with the License.
// You may obtain a copy of the License at
//
//     http://www.apache.org/licenses/LICENSE-2.0
//
// Unless required by applicable law or agreed to in writing, software
// distributed under the License is distributed on an "AS IS" BASIS,
// WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
// See the License for the specific language governing permissions and
// limitations under the License.

package push

import (
	"fmt"
	"net/http"
	"strings"

	"github.com/caddyserver/caddy/v2"
	"github.com/caddyserver/caddy/v2/modules/caddyhttp"
	"github.com/caddyserver/caddy/v2/modules/caddyhttp/headers"
	"go.uber.org/zap"
)

func init() {
	caddy.RegisterModule(Handler{})
}

// Handler is a middleware for manipulating the request body.
type Handler struct {
	Resources []Resource         `json:"resources,omitempty"`
	Headers   *headers.HeaderOps `json:"headers,omitempty"`

	logger *zap.Logger
}

// CaddyModule returns the Caddy module information.
func (Handler) CaddyModule() caddy.ModuleInfo {
	return caddy.ModuleInfo{
		ID:  "http.handlers.push",
		New: func() caddy.Module { return new(Handler) },
	}
}

// Provision sets up h.
func (h *Handler) Provision(ctx caddy.Context) error {
	h.logger = ctx.Logger(h)
	if h.Headers != nil {
		err := h.Headers.Provision(ctx)
		if err != nil {
			return fmt.Errorf("provisioning header operations: %v", err)
		}
	}
	return nil
}

func (h Handler) ServeHTTP(w http.ResponseWriter, r *http.Request, next caddyhttp.Handler) error {
	pusher, ok := w.(http.Pusher)
	if !ok {
		return next.ServeHTTP(w, r)
	}

	// short-circuit recursive pushes
	if _, ok := r.Header[pushHeader]; ok {
		return next.ServeHTTP(w, r)
	}

	repl := r.Context().Value(caddy.ReplacerCtxKey).(*caddy.Replacer)

	// create header for push requests
	hdr := h.initializePushHeaders(r, repl)

	// push first!
	for _, resource := range h.Resources {
		h.logger.Debug("pushing resource",
			zap.String("uri", r.RequestURI),
			zap.String("push_method", resource.Method),
			zap.String("push_target", resource.Target),
			zap.Object("push_headers", caddyhttp.LoggableHTTPHeader(hdr)))
		err := pusher.Push(repl.ReplaceAll(resource.Target, "."), &http.PushOptions{
			Method: resource.Method,
			Header: hdr,
		})
		if err != nil {
			// usually this means either that push is not
			// supported or concurrent streams are full
			break
		}
	}

	// serve only after pushing!
	if err := next.ServeHTTP(w, r); err != nil {
		return err
	}

	// finally, push any resources described by Link fields that were
	// written to the response header, only if another push handler
	// hasn't already done so
	if links, ok := w.Header()["Link"]; ok {
		if val := caddyhttp.GetVar(r.Context(), pushedLink); val == nil {
			h.logger.Debug("pushing Link resources", zap.Strings("linked", links))
			caddyhttp.SetVar(r.Context(), pushedLink, true)
			h.servePreloadLinks(pusher, hdr, links)
		}
	}

	return nil
}

// Resource represents a request for a resource to push.
type Resource struct {
	// Method is the request method, which must be GET or HEAD.
	// Default is GET.
	Method string `json:"method,omitempty"`

	// Target is the path to the resource being pushed.
	Target string `json:"target,omitempty"`
}

func (h Handler) initializePushHeaders(r *http.Request, repl *caddy.Replacer) http.Header {
	hdr := make(http.Header)

	// prevent recursive pushes
	hdr.Set(pushHeader, "1")

	// set initial header fields; since exactly how headers should
	// be implemented for server push is not well-understood, we
	// are being conservative for now like httpd is:
	// https://httpd.apache.org/docs/2.4/en/howto/http2.html#push
	// we only copy some well-known, safe headers that are likely
	// crucial when requesting certain kinds of content
	for _, fieldName := range safeHeaders {
		if vals, ok := r.Header[fieldName]; ok {
			hdr[fieldName] = vals
		}
	}

	// user can customize the push request headers
	if h.Headers != nil {
		h.Headers.ApplyTo(hdr, repl)
	}

	return hdr
}

// servePreloadLinks parses Link headers from upstream and pushes
// resources described by them. If a resource has the "nopush"
// attribute or describes an external entity (meaning, the resource
// URI includes a scheme), it will not be pushed.
func (h Handler) servePreloadLinks(pusher http.Pusher, hdr http.Header, resources []string) {
outer:
	for _, resource := range resources {
		for _, resource := range parseLinkHeader(resource) {
			if _, ok := resource.params["nopush"]; ok {
				continue
			}
			if h.isRemoteResource(resource.uri) {
				continue
			}
			err := pusher.Push(resource.uri, &http.PushOptions{
				Header: hdr,
			})
			if err != nil {
				break outer
			}
		}
	}
}

// isRemoteResource returns true if resource starts with
// a scheme or is a protocol-relative URI.
func (Handler) isRemoteResource(resource string) bool {
	return strings.HasPrefix(resource, "//") ||
		strings.HasPrefix(resource, "http://") ||
		strings.HasPrefix(resource, "https://")
}

// safeHeaders is a list of header fields that are
// safe to copy to push requests implicitly. It is
// assumed that requests for certain kinds of content
// would fail without these fields present.
var safeHeaders = []string{
	"Accept-Encoding",
	"Accept-Language",
	"Accept",
	"Cache-Control",
	"User-Agent",
}

// pushHeader is a header field that gets added to push requests
// in order to avoid recursive/infinite pushes.
const pushHeader = "X-Caddy-Push"

// pushedLink is the key for the variable on the request
// context that we use to remember whether we have already
// pushed resources from Link headers yet; otherwise, if
// multiple push handlers are invoked, it would repeat the
// pushing of Link headers.
const pushedLink = "http.handlers.push.pushed_link"

// Interface guards
var (
	_ caddy.Provisioner           = (*Handler)(nil)
	_ caddyhttp.MiddlewareHandler = (*Handler)(nil)
)
