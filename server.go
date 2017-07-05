package zenrpc

import (
	"context"
	"encoding/json"
	"io/ioutil"
	"net/http"
	"strings"
	"sync"
	"unicode"

	"github.com/sergeyfast/zenrpc/smd"
)

type contextKey string

const (
	// defaultBatchMaxLen is default value of BatchMaxLen option in rpc Server options
	defaultBatchMaxLen = 10

	// defaultTargetUrl is default value for SMD target url.
	defaultTargetUrl = "/"

	// context key for http.Request object
	requestKey contextKey = "request"

	// context key for namespace
	namespaceKey contextKey = "namespace"
)

type MiddlewareFunc func(InvokeFunc) InvokeFunc

// InvokeFunc is a function for processing single JSON-RPC 2.0 Request after validation and parsing.
type InvokeFunc func(context.Context, string, json.RawMessage) Response

// Invoker implements service handler.
type Invoker interface {
	Invoke(ctx context.Context, method string, params json.RawMessage) Response
	SMD() smd.ServiceInfo
}

// Service is as struct for discovering JSON-RPC 2.0 services for zenrpc generator cmd.
type Service struct{}

// Options is options for JSON-RPC 2.0 Server
type Options struct {
	// BatchMaxLen sets maximum quantity of requests in single batch
	BatchMaxLen int

	// TargetUrl is RPC endpoint.
	TargetUrl string

	// ExposeSMD exposes SMD schema with ?smd GET parameter.
	ExposeSMD bool
}

// Server is JSON-RPC 2.0 Server.
type Server struct {
	services   map[string]Invoker
	options    Options
	middleware []MiddlewareFunc
}

// NewServer returns new JSON-RPC 2.0 Server.
func NewServer(opts Options) Server {
	// For safety reasons we do not allowing to much requests in batch
	if opts.BatchMaxLen == 0 {
		opts.BatchMaxLen = defaultBatchMaxLen
	}

	if opts.TargetUrl == "" {
		opts.TargetUrl = defaultTargetUrl
	}

	return Server{
		services: make(map[string]Invoker),
		options:  opts,
	}
}

// Use registers middleware.
func (s *Server) Use(m ...MiddlewareFunc) {
	s.middleware = append(s.middleware, m...)
}

// Register registers new service for given namespace. For public namespace use empty string.
func (s *Server) Register(namespace string, service Invoker) {
	s.services[namespace] = service
}

// process process JSON-RPC 2.0 message, invokes correct method for namespace and returns JSON-RPC 2.0 Response.
func (s *Server) process(ctx context.Context, message json.RawMessage) interface{} {
	requests := []Request{}
	// parsing batch requests
	batch := isBatch(message)

	// making not batch request looks like batch to simplify further code
	if !batch {
		message = append(append([]byte{'['}, message...), ']')
	}

	// unmarshal request(s)
	if err := json.Unmarshal(message, &requests); err != nil {
		return NewResponseError(nil, ParseError, "", nil)
	}

	// if there no requests to process
	if len(requests) == 0 {
		return NewResponseError(nil, InvalidRequest, "", nil)
	} else if len(requests) > s.options.BatchMaxLen {
		return NewResponseError(nil, InvalidRequest, "", "max requests length in batch exceeded")
	}

	// process single request: if request single and not notification  - just run it and return result
	if !batch && requests[0].ID != nil {
		return s.processRequest(ctx, requests[0])
	}

	// process batch requests
	if res := s.processBatch(ctx, requests); len(res) > 0 {
		return res
	}

	return nil
}

// processBatch process batch requests with context.
func (s Server) processBatch(ctx context.Context, requests []Request) []Response {
	reqLen := len(requests)

	// running requests in batch asynchronously
	respChan := make(chan Response, reqLen)
	var wg sync.WaitGroup
	wg.Add(reqLen)

	for _, req := range requests {
		// running request in goroutine
		go func(req Request) {
			if req.ID == nil {
				// ignoring response if request is notification
				wg.Done()
				s.processRequest(ctx, req)
			} else {
				respChan <- s.processRequest(ctx, req)
				wg.Done()
			}
		}(req)
	}

	// TODO what if one of requests freezes?
	// waiting to complete
	wg.Wait()
	close(respChan)

	// collecting responses
	responses := make([]Response, 0, reqLen)
	for r := range respChan {
		responses = append(responses, r)
	}

	// no responses -> all requests are notifications
	if len(responses) == 0 {
		return nil
	}
	return responses
}

// processRequest processes a single request in service invoker
func (s Server) processRequest(ctx context.Context, req Request) Response {
	// checks for json-rpc version and method
	if req.Version != Version || req.Method == "" {
		return NewResponseError(req.ID, InvalidRequest, "", nil)
	}

	// convert method to lower and find namespace
	lowerM := strings.ToLower(req.Method)
	sp := strings.SplitN(lowerM, ".", 2)
	namespace, method := "", lowerM
	if len(sp) == 2 {
		namespace, method = sp[0], sp[1]
	}

	if _, ok := s.services[namespace]; !ok {
		return NewResponseError(req.ID, MethodNotFound, "", nil)
	}

	// set namespace to context
	ctx = newNamespaceContext(ctx, namespace)

	// set middleware to func
	f := InvokeFunc(s.services[namespace].Invoke)
	for _, m := range s.middleware {
		f = m(f)
	}

	// invoke func with middleware
	resp := f(ctx, method, req.Params)
	resp.ID = req.ID

	return resp
}

// ServeHTTP process JSON-RPC 2.0 requests via HTTP.
func (s Server) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	// check for smd parameter and server settings and write schema if all conditions met,
	if _, ok := r.URL.Query()["smd"]; ok && s.options.ExposeSMD {
		b, _ := json.Marshal(s.SMD())
		w.Write(b)
		return
	}

	b, err := ioutil.ReadAll(r.Body)
	var data interface{}

	if err != nil {
		data = NewResponseError(nil, ParseError, "", nil)
	} else {
		data = s.process(newRequestContext(r.Context(), r), b)
	}

	// if responses is empty -> all requests are notifications -> exit immediately
	if data == nil {
		return
	}

	// marshals data and write it to client.
	if resp, err := json.Marshal(data); err != nil {
		w.WriteHeader(http.StatusInternalServerError)
	} else if _, err := w.Write(resp); err != nil {
		w.WriteHeader(http.StatusInternalServerError)
	}

	return
}

// isBatch checks json message if it array or object
func isBatch(message json.RawMessage) bool {
	for _, b := range message {
		if unicode.IsSpace(rune(b)) {
			continue
		}

		if b == '[' {
			return true
		}
		break
	}

	return false
}

// SMD returns Service Mapping Description object with all registered methods.
func (s Server) SMD() smd.Schema {
	sch := smd.Schema{
		Transport:   "POST",
		Envelope:    "JSON-RPC-2.0",
		SMDVersion:  "2.0",
		ContentType: "application/json",
		Target:      s.options.TargetUrl,
		Services:    make(map[string]smd.Service),
	}

	for n, v := range s.services {
		info, namespace := v.SMD(), ""
		if n != "" {
			namespace = n + "."
		}

		for m, d := range info.Methods {
			method := namespace + m
			sch.Services[method] = d
			sch.Description += info.Description // TODO formatting
		}
	}

	return sch
}

// newRequestContext creates new context with http.Request.
func newRequestContext(ctx context.Context, req *http.Request) context.Context {
	return context.WithValue(ctx, requestKey, req)
}

// RequestFromContext returns http.Request from context.
func RequestFromContext(ctx context.Context) (*http.Request, bool) {
	r, ok := ctx.Value(requestKey).(*http.Request)
	return r, ok
}

// newNamespaceContext creates new context with current method namespace.
func newNamespaceContext(ctx context.Context, namespace string) context.Context {
	return context.WithValue(ctx, namespaceKey, namespace)
}

// NamespaceFromContext returns method's namespace from context.
func NamespaceFromContext(ctx context.Context) string {
	if r, ok := ctx.Value(namespaceKey).(string); ok {
		return r
	}

	return ""
}
