package main

import (
	"bytes"
	"encoding/base64"
	"encoding/json"
	"flag"
	"io"
	"log"
	"net"
	"net/http"
	"net/http/httputil"
	"net/url"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"time"
)

const CacheHit = "HIT"

var (
	upstream    string
	upstreamUrl *url.URL

	cacheDir string
	cache    cacheManager
)

func init() {
	flag.StringVar(&upstream, "upstream", "", "Set the `URL` endpoint to proxy from, in the format https://example.com")
	flag.StringVar(&cacheDir, "cache-dir", "cache", "Set the `DIRECTORY` where the cache will be saved")
}

func main() {
	flag.Parse()

	// Detect upstream server to serve from
	if upstream == "" {
		log.Fatalf("Empty upstream URL: use --upstream to set")
	}
	var err error
	upstreamUrl, err = url.Parse(upstream)
	if err != nil {
		log.Fatalf("Invalid upstream URL: %v", err)
	}

	// Initializes the cacheManager
	cache = newFsCache(cacheDir)

	// Intialize roundtripper with caching capabilities, using the cacheManager
	roundTripper := &cachedRoundrip{
		cache: cache,
		host:  upstreamUrl.Host,
		t: http.Transport{
			DialContext: (&net.Dialer{
				Timeout:   120 * time.Second,
				KeepAlive: 30 * time.Second,
				DualStack: true,
			}).DialContext,
			MaxIdleConns:          100,
			IdleConnTimeout:       120 * time.Second,
			ExpectContinueTimeout: 30 * time.Second,
		},
	}

	p := httputil.NewSingleHostReverseProxy(upstreamUrl)
	p.Director = prepareRequest
	p.Transport = roundTripper
	p.ModifyResponse = roundTripper.cacheResponse
	log.Fatal(http.ListenAndServe(":8080", p))
}

func prepareRequest(r *http.Request) {
	r.URL.Scheme = upstreamUrl.Scheme
	r.URL.Host = upstreamUrl.Host
	r.Host = upstreamUrl.Host
}

func cacheKey(uri string) string {
	return base64.URLEncoding.EncodeToString([]byte(uri))
}

// cachedRountrip retrieves serves cached data if available.
type cachedRoundrip struct {
	t     http.Transport
	cache cacheManager
	host  string
}

func (c *cachedRoundrip) cacheResponse(w *http.Response) error {
	// Replace location header from upstream
	if w.Header.Get("location") != "" {
		l := w.Header.Get("location")
		l = strings.ReplaceAll(l, upstream, "")
		l = strings.ReplaceAll(l, upstreamUrl.Host, "")
		w.Header.Set("location", l)
	}
	if w.StatusCode != 200 || w.Header.Get("x-cache") == CacheHit {
		return nil
	}

	// TODO(ronoaldo): improve memory usage here... if file is too big
	// it will read it all in-memory.
	buff := &bytes.Buffer{}
	tee := io.TeeReader(w.Body, buff)
	k := cacheKey(w.Request.RequestURI)
	if err := c.cache.Put(k, io.NopCloser(tee), w.Request.Header); err != nil {
		return err
	}

	// Wrap the buffer again into the response so this one is
	// properly served.
	w.Body.Close()
	w.Body = io.NopCloser(buff)
	return nil
}

func (c *cachedRoundrip) RoundTrip(r *http.Request) (w *http.Response, err error) {
	var uri = r.URL.RequestURI()
	k := cacheKey(uri)

	log.Printf("[transport] Request '%v' => '%v'", uri, k)
	// log.Printf("[transport] Request headers: %#v", r.Header)

	b, h, err := c.cache.Get(k)
	if err == nil {
		log.Printf("[transport] Returning data from cache")
		h.Set("x-cache", CacheHit)
		w = &http.Response{
			Request:    r,
			Body:       b,
			Header:     h,
			Status:     "200 OK",
			StatusCode: 200,
		}
		return w, nil
	} else {
		log.Printf("[transport] Cache miss (err=%v)", err)
	}

	w, err = c.t.RoundTrip(r)
	if err != nil {
		log.Printf("[transport] Error returned during request: %v", err)
		return nil, err
	}

	log.Printf("[transport] Returned status: %v %v", w.StatusCode, w.Status)
	return w, err
}

// cacheManager is a helper interface to abstract the FS cache
type cacheManager interface {
	// Put stores a file and relevant HTTP headers.
	Put(key string, blob io.ReadCloser, h http.Header) error

	// Get retrieves both file and metadata.
	Get(key string) (blob io.ReadCloser, h http.Header, err error)

	// Flush expires the cached file from underlying storage.
	Flush(key string) error
}

// fsCache cache files in the local filesystem at dir.
type fsCache struct {
	dir string
}

// Ensures we implement cacheManager interface
var _ cacheManager = &fsCache{}

func newFsCache(dir string) *fsCache {
	// Try to initialize the cache directory
	if err := os.MkdirAll("cache/", 0777); err != nil {
		log.Printf("[fscache] error initializing directory: %v", err)
	}
	return &fsCache{dir: dir}
}

func (c *fsCache) Put(key string, blob io.ReadCloser, h http.Header) (err error) {
	key = filepath.Join(c.dir, key)
	log.Printf("[fscache] Storing key=%v", key)
	// Save blob contents
	fd, err := os.Create(key)
	if err != nil {
		return err
	}
	defer fd.Close()
	if _, err = io.Copy(fd, blob); err != nil {
		return err
	}

	// Save headers
	aux := make(http.Header)
	for _, k := range []string{"content-type", "content-length"} {
		if h.Get(k) != "" {
			aux.Set(k, h.Get(k))
		}
	}
	hfd, err := os.Create(key + ".headers")
	if err != nil {
		return err
	}
	defer hfd.Close()
	if err = json.NewEncoder(hfd).Encode(aux); err != nil {
		return err
	}

	return nil
}

func (c *fsCache) Get(key string) (blob io.ReadCloser, h http.Header, err error) {
	key = filepath.Join(c.dir, key)
	b, err := os.ReadFile(key)
	if err != nil {
		log.Printf("[fscache] error opening cache key=%v: %v", key, err)
		return
	}
	hb, err := os.ReadFile(key + ".headers")
	if err != nil {
		log.Printf("[fscache] error opening cache headers=%v.headers: %v", key, err)
		return
	}
	h = make(http.Header)
	if err = json.Unmarshal(hb, &h); err != nil {
		log.Printf("[fscache] error decoding headers: %v", err)
		return
	}
	// If upstream did not provide valid headers, or we failed to store them,
	// fix the content type and length ones to avoid 502 bad gateway.
	if h.Get("content-length") == "" {
		h.Set("content-length", strconv.Itoa(len(b)))
	}
	log.Printf("[fscache] Cache hit!")
	blob = io.NopCloser(bytes.NewBuffer(b))
	return blob, h, err
}

func (c *fsCache) Flush(key string) (err error) {
	key = filepath.Join(c.dir, key)
	return os.Remove(key)
}
