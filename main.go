package main

import (
	"crypto/tls"
	"log"
	"os"
	"strconv"
	"strings"
	"time"

	"github.com/valyala/fasthttp"
)

func getenvInt(name string, def int) int {
	v := os.Getenv(name)
	if v == "" {
		return def
	}
	i, err := strconv.Atoi(v)
	if err != nil {
		return def
	}
	return i
}

func getenv(name, def string) string {
	v := os.Getenv(name)
	if v == "" {
		return def
	}
	return v
}

var (
	timeout = getenvInt("TIMEOUT", 10)   // seconds
	retries = getenvInt("RETRIES", 3)    // retry attempts
	port    = getenv("PORT", "10000")    // Render supplies PORT; default fallback
	client  *fasthttp.Client
)

func main() {
	// create HTTP client with reasonable defaults
	client = &fasthttp.Client{
		ReadTimeout:        time.Duration(timeout) * time.Second,
		MaxIdleConnDuration: 60 * time.Second,
		MaxConnsPerHost:     100,
		TLSConfig: &tls.Config{
			MinVersion: tls.VersionTLS12,
		},
	}

	h := requestHandler
	if err := fasthttp.ListenAndServe(":"+port, h); err != nil {
		log.Fatalf("ListenAndServe error: %v", err)
	}
}

func requestHandler(ctx *fasthttp.RequestCtx) {
	// If KEY is set, require PROXYKEY header
	if val, ok := os.LookupEnv("KEY"); ok {
		if string(ctx.Request.Header.Peek("PROXYKEY")) != val {
			ctx.SetStatusCode(407)
			ctx.SetBody([]byte("Missing or invalid PROXYKEY header."))
			return
		}
	}

	// Must have at least two parts after first slash: e.g. marketplace/asset/ID
	raw := string(ctx.Request.Header.RequestURI())
	// raw usually starts with path like "/marketplace/asset/123?x=1"
	if len(raw) == 0 {
		ctx.SetStatusCode(400)
		ctx.SetBody([]byte("URL format invalid."))
		return
	}
	// remove leading slash
	if raw[0] == '/' {
		raw = raw[1:]
	}
	parts := strings.SplitN(raw, "/", 2)
	if len(parts) < 2 {
		ctx.SetStatusCode(400)
		ctx.SetBody([]byte("URL format invalid."))
		return
	}

	// Perform the proxied request with retries
	resp := makeRequest(ctx, 1)
	defer fasthttp.ReleaseResponse(resp)

	// Copy response body and status back to client
	ctx.SetStatusCode(resp.StatusCode())
	ctx.SetBody(resp.Body())

	// Copy response headers (avoid hop-by-hop headers)
	resp.Header.VisitAll(func(k, v []byte) {
		key := strings.ToLower(string(k))
		switch key {
		case "connection", "proxy-connection", "keep-alive", "transfer-encoding", "upgrade", "proxy-authenticate", "proxy-authorization", "te", "trailer", "trailers":
			// skip hop-by-hop
		default:
			ctx.Response.Header.Set(string(k), string(v))
		}
	})
}

func makeRequest(ctx *fasthttp.RequestCtx, attempt int) *fasthttp.Response {
	if attempt > retries {
		r := fasthttp.AcquireResponse()
		r.SetStatusCode(500)
		r.SetBody([]byte("Proxy failed to connect. Please try again."))
		return r
	}

	// Build target URL: https://{subdomain}.roblox.com/{rest}
	raw := string(ctx.Request.Header.RequestURI())
	if raw != "" && raw[0] == '/' {
		raw = raw[1:]
	}
	parts := strings.SplitN(raw, "/", 2)
	targetHost := parts[0] + ".roblox.com"
	targetPath := ""
	if len(parts) > 1 {
		targetPath = parts[1]
	}

	targetURL := "https://" + targetHost + "/" + targetPath
	log.Printf("Proxy attempt %d -> %s", attempt, targetURL)

	// Create request
	req := fasthttp.AcquireRequest()
	defer fasthttp.ReleaseRequest(req)
	req.SetRequestURI(targetURL)
	req.Header.SetMethod(string(ctx.Method()))
	// Copy headers from client request but skip hop-by-hop and proxy headers
	ctx.Request.Header.VisitAll(func(k, v []byte) {
		key := strings.ToLower(string(k))
		switch key {
		case "connection", "proxy-connection", "keep-alive", "transfer-encoding", "upgrade", "proxy-authenticate", "proxy-authorization", "te", "trailer", "trailers":
			// skip
		case "host":
			// we'll set host explicitly below
		default:
			req.Header.Set(string(k), string(v))
		}
	})
	// set Host correctly
	req.Header.Set("Host", targetHost)
	// set a sensible user agent
	req.Header.Set("User-Agent", "RoProxy/1.0")
	// remove any Roblox-Id header that might interfere
	req.Header.Del("Roblox-Id")

	// copy body (works for GET with empty body too)
	req.SetBody(ctx.Request.Body())

	// Acquire response and do the request
	resp := fasthttp.AcquireResponse()
	err := client.Do(req, resp)
	if err != nil {
		// log full error so Render shows the reason
		log.Printf("Request error (attempt %d): %v", attempt, err)
		fasthttp.ReleaseResponse(resp)
		// simple backoff before retrying
		time.Sleep(time.Duration(attempt) * 300 * time.Millisecond)
		return makeRequest(ctx, attempt+1)
	}

	return resp
}
