// Package rcserver implements the HTTP endpoint to serve the remote control
package rcserver

import (
	"encoding/json"
	"mime"
	"net/http"
	"net/url"
	"regexp"
	"sort"
	"strings"

	"github.com/ncw/rclone/cmd/serve/httplib"
	"github.com/ncw/rclone/cmd/serve/httplib/serve"
	"github.com/ncw/rclone/fs"
	"github.com/ncw/rclone/fs/cache"
	"github.com/ncw/rclone/fs/config"
	"github.com/ncw/rclone/fs/list"
	"github.com/ncw/rclone/fs/rc"
	"github.com/pkg/errors"
	"github.com/skratchdot/open-golang/open"
)

// Start the remote control server if configured
//
// If the server wasn't configured the *Server returned may be nil
func Start(opt *rc.Options) (*Server, error) {
	if opt.Enabled {
		// Serve on the DefaultServeMux so can have global registrations appear
		s := newServer(opt, http.DefaultServeMux)
		return s, s.Serve()
	}
	return nil, nil
}

// Server contains everything to run the rc server
type Server struct {
	*httplib.Server
	files http.Handler
	opt   *rc.Options
}

func newServer(opt *rc.Options, mux *http.ServeMux) *Server {
	s := &Server{
		Server: httplib.NewServer(mux, &opt.HTTPOptions),
		opt:    opt,
	}
	mux.HandleFunc("/", s.handler)

	// Add some more mime types which are often missing
	_ = mime.AddExtensionType(".wasm", "application/wasm")
	_ = mime.AddExtensionType(".js", "application/javascript")

	// File handling
	if opt.Files != "" {
		fs.Logf(nil, "Serving files from %q", opt.Files)
		s.files = http.FileServer(http.Dir(opt.Files))
	}
	return s
}

// Serve runs the http server in the background.
//
// Use s.Close() and s.Wait() to shutdown server
func (s *Server) Serve() error {
	err := s.Server.Serve()
	if err != nil {
		return err
	}
	fs.Logf(nil, "Serving remote control on %s", s.URL())
	// Open the files in the browser if set
	if s.files != nil {
		openURL, err := url.Parse(s.URL())
		if err != nil {
			return errors.Wrap(err, "invalid serving URL")
		}
		// Add username, password into the URL if they are set
		user, pass := s.opt.HTTPOptions.BasicUser, s.opt.HTTPOptions.BasicPass
		if user != "" || pass != "" {
			openURL.User = url.UserPassword(user, pass)
		}
		_ = open.Start(openURL.String())
	}
	return nil
}

// writeError writes a formatted error to the output
func writeError(path string, in rc.Params, w http.ResponseWriter, err error, status int) {
	fs.Errorf(nil, "rc: %q: error: %v", path, err)
	// Adjust the error return for some well known errors
	errOrig := errors.Cause(err)
	switch {
	case errOrig == fs.ErrorDirNotFound || errOrig == fs.ErrorObjectNotFound:
		status = http.StatusNotFound
	case rc.IsErrParamInvalid(err) || rc.IsErrParamNotFound(err):
		status = http.StatusBadRequest
	}
	w.WriteHeader(status)
	err = rc.WriteJSON(w, rc.Params{
		"status": status,
		"error":  err.Error(),
		"input":  in,
		"path":   path,
	})
	if err != nil {
		// can't return the error at this point
		fs.Errorf(nil, "rc: failed to write JSON output: %v", err)
	}
}

// handler reads incoming requests and dispatches them
func (s *Server) handler(w http.ResponseWriter, r *http.Request) {
	path := strings.TrimLeft(r.URL.Path, "/")

	w.Header().Add("Access-Control-Allow-Origin", "*")

	// echo back access control headers client needs
	reqAccessHeaders := r.Header.Get("Access-Control-Request-Headers")
	w.Header().Add("Access-Control-Allow-Headers", reqAccessHeaders)

	switch r.Method {
	case "POST":
		s.handlePost(w, r, path)
	case "OPTIONS":
		s.handleOptions(w, r, path)
	case "GET", "HEAD":
		s.handleGet(w, r, path)
	default:
		writeError(path, nil, w, errors.Errorf("method %q not allowed", r.Method), http.StatusMethodNotAllowed)
		return
	}
}

func (s *Server) handlePost(w http.ResponseWriter, r *http.Request, path string) {
	contentType := r.Header.Get("Content-Type")

	values := r.URL.Query()
	if contentType == "application/x-www-form-urlencoded" {
		// Parse the POST and URL parameters into r.Form, for others r.Form will be empty value
		err := r.ParseForm()
		if err != nil {
			writeError(path, nil, w, errors.Wrap(err, "failed to parse form/URL parameters"), http.StatusBadRequest)
			return
		}
		values = r.Form
	}

	// Read the POST and URL parameters into in
	in := make(rc.Params)
	for k, vs := range values {
		if len(vs) > 0 {
			in[k] = vs[len(vs)-1]
		}
	}

	// Parse a JSON blob from the input
	if contentType == "application/json" {
		err := json.NewDecoder(r.Body).Decode(&in)
		if err != nil {
			writeError(path, in, w, errors.Wrap(err, "failed to read input JSON"), http.StatusBadRequest)
			return
		}
	}

	// Find the call
	call := rc.Calls.Get(path)
	if call == nil {
		writeError(path, in, w, errors.Errorf("couldn't find method %q", path), http.StatusNotFound)
		return
	}

	// Check to see if it requires authorisation
	if !s.opt.NoAuth && call.AuthRequired && !s.UsingAuth() {
		writeError(path, in, w, errors.Errorf("authentication must be set up on the rc server to use %q or the --rc-no-auth flag must be in use", path), http.StatusForbidden)
		return
	}

	// Check to see if it is async or not
	isAsync, err := in.GetBool("_async")
	if rc.NotErrParamNotFound(err) {
		writeError(path, in, w, err, http.StatusBadRequest)
		return
	}

	delete(in, "_async") // remove the async parameter after parsing so vfs operations don't get confused

	fs.Debugf(nil, "rc: %q: with parameters %+v", path, in)
	var out rc.Params
	if isAsync {
		out, err = rc.StartJob(call.Fn, in)
	} else {
		out, err = call.Fn(r.Context(), in)
	}
	if err != nil {
		writeError(path, in, w, err, http.StatusInternalServerError)
		return
	}
	if out == nil {
		out = make(rc.Params)
	}

	fs.Debugf(nil, "rc: %q: reply %+v: %v", path, out, err)
	err = rc.WriteJSON(w, out)
	if err != nil {
		// can't return the error at this point
		fs.Errorf(nil, "rc: failed to write JSON output: %v", err)
	}
}

func (s *Server) handleOptions(w http.ResponseWriter, r *http.Request, path string) {
	w.WriteHeader(http.StatusOK)
}

func (s *Server) serveRoot(w http.ResponseWriter, r *http.Request) {
	remotes := config.FileSections()
	sort.Strings(remotes)
	directory := serve.NewDirectory("", s.HTMLTemplate)
	directory.Title = "List of all rclone remotes."
	q := url.Values{}
	for _, remote := range remotes {
		q.Set("fs", remote)
		directory.AddEntry("["+remote+":]", true)
	}
	directory.Serve(w, r)
}

func (s *Server) serveRemote(w http.ResponseWriter, r *http.Request, path string, fsName string) {
	f, err := cache.Get(fsName)
	if err != nil {
		writeError(path, nil, w, errors.Wrap(err, "failed to make Fs"), http.StatusInternalServerError)
		return
	}
	if path == "" || strings.HasSuffix(path, "/") {
		path = strings.Trim(path, "/")
		entries, err := list.DirSorted(r.Context(), f, false, path)
		if err != nil {
			writeError(path, nil, w, errors.Wrap(err, "failed to list directory"), http.StatusInternalServerError)
			return
		}
		// Make the entries for display
		directory := serve.NewDirectory(path, s.HTMLTemplate)
		for _, entry := range entries {
			_, isDir := entry.(fs.Directory)
			directory.AddEntry(entry.Remote(), isDir)
		}
		directory.Serve(w, r)
	} else {
		path = strings.Trim(path, "/")
		o, err := f.NewObject(r.Context(), path)
		if err != nil {
			writeError(path, nil, w, errors.Wrap(err, "failed to find object"), http.StatusInternalServerError)
			return
		}
		serve.Object(w, r, o)
	}
}

// Match URLS of the form [fs]/remote
var fsMatch = regexp.MustCompile(`^\[(.*?)\](.*)$`)

func (s *Server) handleGet(w http.ResponseWriter, r *http.Request, path string) {
	// Look to see if this has an fs in the path
	match := fsMatch.FindStringSubmatch(path)
	switch {
	case match != nil && s.opt.Serve:
		// Serve /[fs]/remote files
		s.serveRemote(w, r, match[2], match[1])
		return
	case path == "*" && s.opt.Serve:
		// Serve /* as the remote listing
		s.serveRoot(w, r)
		return
	case s.files != nil:
		// Serve the files
		s.files.ServeHTTP(w, r)
		return
	case path == "" && s.opt.Serve:
		// Serve the root as a remote listing
		s.serveRoot(w, r)
		return
	}
	http.Error(w, http.StatusText(http.StatusNotFound), http.StatusNotFound)
}
