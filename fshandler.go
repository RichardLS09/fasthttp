package fasthttp

import (
	"bytes"
	"errors"
	"fmt"
	"html"
	"io"
	"mime"
	"os"
	"sort"
	"strings"
	"sync"
	"time"
)

// FSHandlerCacheDuration is the duration for caching open file handles
// by FSHandler.
const FSHandlerCacheDuration = 10 * time.Second

// FSHandler returns request handler serving static files from
// the given root folder.
//
// stripSlashes indicates how many leading slashes must be stripped
// from requested path before searching requested file in the root folder.
// Examples:
//
//   * stripSlashes = 0, original path: "/foo/bar", result: "/foo/bar"
//   * stripSlashes = 1, original path: "/foo/bar", result: "/bar"
//   * stripSlashes = 2, original path: "/foo/bar", result: ""
//
// FSHandler caches requested file handles for FSHandlerCacheDuration.
// Make sure your program has enough 'max open files' limit aka
// 'ulimit -n' if root folder contains many files.
//
// Do not create multiple FSHandler instances for the same (root, stripSlashes)
// arguments - just reuse a single instance. Otherwise goroutine leak
// will occur.
func FSHandler(root string, stripSlashes int) RequestHandler {
	// strip trailing slashes from the root path
	for len(root) > 0 && root[len(root)-1] == '/' {
		root = root[:len(root)-1]
	}

	// serve files from the current working directory
	if len(root) == 0 {
		root = "."
	}

	if stripSlashes < 0 {
		stripSlashes = 0
	}

	h := &fsHandler{
		root:         root,
		stripSlashes: stripSlashes,
		cache:        make(map[string]*fsFile),
	}
	go func() {
		for {
			time.Sleep(FSHandlerCacheDuration / 2)
			h.cleanCache()
		}
	}()
	return h.handleRequest
}

type fsHandler struct {
	root         string
	stripSlashes int
	cache        map[string]*fsFile
	cacheLock    sync.Mutex
}

type fsFile struct {
	f             *os.File
	dirIndex      []byte
	contentType   string
	contentLength int
	t             time.Time
}

func (ff *fsFile) Reader() io.Reader {
	v := fsFileReaderPool.Get()
	if v == nil {
		r := &fsFileReader{
			f:        ff.f,
			dirIndex: ff.dirIndex,
		}
		r.v = r
		return r
	}
	r := v.(*fsFileReader)
	r.f = ff.f
	r.dirIndex = ff.dirIndex
	if r.offset > 0 {
		panic("BUG: fsFileReader with non-nil offset found in the pool")
	}
	return r
}

var fsFileReaderPool sync.Pool

type fsFileReader struct {
	f        *os.File
	dirIndex []byte
	offset   int64

	v interface{}
}

func (r *fsFileReader) Close() error {
	r.f = nil
	r.dirIndex = nil
	r.offset = 0
	fsFileReaderPool.Put(r.v)
	return nil
}

func (r *fsFileReader) Read(p []byte) (int, error) {
	if r.f != nil {
		n, err := r.f.ReadAt(p, r.offset)
		r.offset += int64(n)
		return n, err
	}

	if r.offset == int64(len(r.dirIndex)) {
		return 0, io.EOF
	}
	n := copy(p, r.dirIndex[r.offset:])
	r.offset += int64(n)
	return n, nil
}

func (h *fsHandler) cleanCache() {
	t := time.Now()
	h.cacheLock.Lock()
	for k, v := range h.cache {
		if t.Sub(v.t) > FSHandlerCacheDuration {
			if v.f != nil {
				v.f.Close()
			}
			delete(h.cache, k)
		}
	}
	h.cacheLock.Unlock()
}

func (h *fsHandler) handleRequest(ctx *RequestCtx) {
	path := ctx.Path()
	path = stripPathSlashes(path, h.stripSlashes)

	if n := bytes.IndexByte(path, 0); n >= 0 {
		ctx.Logger().Printf("cannot serve path with nil byte at position %d: %q", n, path)
		ctx.Error("Are you a hacker?", StatusBadRequest)
		return
	}

	h.cacheLock.Lock()
	ff, ok := h.cache[string(path)]
	h.cacheLock.Unlock()

	if !ok {
		filePath := h.root + string(path)
		var err error
		ff, err = openFSFile(filePath)
		if err == errDirIndexRequired {
			ff, err = createDirIndex(ctx.URI(), filePath)
			if err != nil {
				ctx.Logger().Printf("Cannot create index for directory %q: %s", filePath, err)
				ctx.Error("Cannot create directory index", StatusNotFound)
				return
			}
		} else if err != nil {
			ctx.Logger().Printf("cannot open file %q: %s", filePath, err)
			ctx.Error("Cannot open requested path", StatusNotFound)
			return
		}

		h.cacheLock.Lock()
		h.cache[string(path)] = ff
		h.cacheLock.Unlock()
	}

	ctx.SetBodyStream(ff.Reader(), ff.contentLength)
	ctx.SetContentType(ff.contentType)
}

var errDirIndexRequired = errors.New("directory index required")

func createDirIndex(base *URI, filePath string) (*fsFile, error) {
	var buf bytes.Buffer
	w := &buf

	basePathEscaped := html.EscapeString(string(base.Path()))
	fmt.Fprintf(w, "<html><head><title>%s</title></head><body>", basePathEscaped)
	fmt.Fprintf(w, "<h1>%s</h1>", basePathEscaped)
	fmt.Fprintf(w, "<ul>")

	if len(basePathEscaped) > 1 {
		fmt.Fprintf(w, `<li><a href="..">..</a></li>`)
	}

	f, err := os.Open(filePath)
	if err != nil {
		return nil, err
	}

	filenames, err := f.Readdirnames(0)
	f.Close()
	if err != nil {
		return nil, err
	}

	var u URI
	base.CopyTo(&u)

	sort.Sort(sort.StringSlice(filenames))
	for _, name := range filenames {
		u.Update(name)
		pathEscaped := html.EscapeString(string(u.Path()))
		fmt.Fprintf(w, `<li><a href="%s">%s</a></li>`, pathEscaped, html.EscapeString(name))
	}

	fmt.Fprintf(w, "</ul></body></html>")
	dirIndex := w.Bytes()

	ff := &fsFile{
		dirIndex:      dirIndex,
		contentType:   "text/html",
		contentLength: len(dirIndex),
	}
	return ff, nil
}

func openFSFile(filePath string) (*fsFile, error) {
	f, err := os.Open(filePath)
	if err != nil {
		return nil, err
	}

	stat, err := f.Stat()
	if err != nil {
		f.Close()
		return nil, err
	}

	if stat.IsDir() {
		f.Close()

		indexPath := filePath + "/index.html"
		ff, err := openFSFile(indexPath)
		if err == nil {
			return ff, nil
		}
		if !os.IsNotExist(err) {
			return nil, err
		}
		return nil, errDirIndexRequired
	}

	n := stat.Size()
	contentLength := int(n)
	if n != int64(contentLength) {
		f.Close()
		return nil, fmt.Errorf("too big file: %d bytes", n)
	}

	ext := fileExtension(filePath)
	contentType := mime.TypeByExtension(ext)

	ff := &fsFile{
		f:             f,
		contentType:   contentType,
		contentLength: contentLength,
	}
	return ff, nil
}

func stripPathSlashes(path []byte, stripSlashes int) []byte {
	// strip leading slashes
	for stripSlashes > 0 && len(path) > 0 {
		if path[0] != '/' {
			panic("BUG: path must start with slash")
		}
		n := bytes.IndexByte(path[1:], '/')
		if n < 0 {
			path = path[:0]
			break
		}
		path = path[n+1:]
		stripSlashes--
	}

	// strip trailing slashes
	for len(path) > 0 && path[len(path)-1] == '/' {
		path = path[:len(path)-1]
	}

	return path
}

func fileExtension(path string) string {
	n := strings.LastIndexByte(path, '.')
	if n < 0 {
		return ""
	}
	return path[n:]
}
