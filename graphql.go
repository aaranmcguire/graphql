// Package graphql provides a low level GraphQL client.
//
//	// create a client (safe to share across requests)
//	client := graphql.NewClient("https://machinebox.io/graphql")
//
//	// make a request
//	req := graphql.NewRequest(`
//	    query ($key: String!) {
//	        items (id:$key) {
//	            field1
//	            field2
//	            field3
//	        }
//	    }
//	`)
//
//	// set any variables
//	req.Var("key", "value")
//
//	// run it and capture the response
//	var respData ResponseStruct
//	if err := client.Run(ctx, req, &respData); err != nil {
//	    log.Fatal(err)
//	}
//
// # Specify client
//
// To specify your own http.Client, use the WithHTTPClient option:
//
//	httpclient := &http.Client{}
//	client := graphql.NewClient("https://machinebox.io/graphql", graphql.WithHTTPClient(httpclient))
package graphql

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"mime/multipart"
	"net/http"
	"strconv"

	"github.com/pkg/errors"
)

// Client is a client for interacting with a GraphQL API.
type Client struct {
	endpoint         string
	httpClient       *http.Client
	useMultipartForm bool

	useMultipartRequestSpec bool

	// closeReq will close the request body immediately allowing for reuse of client
	closeReq bool

	// Log is called with various debug information.
	// To log to standard out, use:
	//  client.Log = func(s string) { log.Println(s) }
	Log func(s string)
}

// NewClient makes a new Client capable of making GraphQL requests.
func NewClient(endpoint string, opts ...ClientOption) *Client {
	c := &Client{
		endpoint: endpoint,
		Log:      func(string) {},
	}
	for _, optionFunc := range opts {
		optionFunc(c)
	}
	if c.httpClient == nil {
		c.httpClient = http.DefaultClient
	}
	return c
}

func (c *Client) logf(format string, args ...interface{}) {
	c.Log(fmt.Sprintf(format, args...))
}

// Run executes the query and unmarshals the response from the data field
// into the provided response object. Pass a nil response object to skip
// response parsing. If the request fails or the server returns an error,
// the first error encountered will be returned.
//
// This function handles different request formats based on the client configuration:
// - If files are included in the request and neither multipart form nor multipart request spec is enabled, it returns an error.
// - If useMultipartForm is enabled, it uses runWithPostFields to send the request.
// - If useMultipartRequestSpec is enabled, it uses runMultipartRequestSpec to send the request.
// - Otherwise, it defaults to using runWithJSON to send the request.
func (c *Client) Run(ctx context.Context, req *Request, resp interface{}) error {
	select {
	case <-ctx.Done():
		return ctx.Err()
	default:
	}
	if len(req.files) > 0 && !(c.useMultipartForm || c.useMultipartRequestSpec) {
		return errors.New("cannot send files with PostFields option")
	}
	if c.useMultipartForm {
		return c.runWithPostFields(ctx, req, resp)
	}
	if c.useMultipartRequestSpec {
		return c.runMultipartRequestSpec(ctx, req, resp)
	}
	return c.runWithJSON(ctx, req, resp)
}

func (c *Client) runWithJSON(ctx context.Context, req *Request, resp interface{}) error {
	var requestBody bytes.Buffer

	// Prepare the request body object
	requestBodyObj := struct {
		Query     string                 `json:"query"`
		Variables map[string]interface{} `json:"variables"`
	}{
		Query:     req.q,
		Variables: req.vars,
	}

	// Encode the request body to JSON
	if err := json.NewEncoder(&requestBody).Encode(requestBodyObj); err != nil {
		return errors.Wrap(err, "failed to encode request body")
	}

	// Log the request details
	c.logf(">> variables: %v", req.vars)
	c.logf(">> query: %s", req.q)

	// Set the request body and content type
	req.body = requestBody
	req.contentType = "application/json; charset=utf-8"

	// Make the HTTP request
	return c.makeRequest(ctx, req, resp)
}

func (c *Client) runWithPostFields(ctx context.Context, req *Request, resp interface{}) error {
	var requestBody bytes.Buffer
	writer := multipart.NewWriter(&requestBody)

	// Write the query field
	if err := writer.WriteField("query", req.q); err != nil {
		return errors.Wrap(err, "failed to write query field")
	}

	// Write the variables field if there are any
	var variablesBuf bytes.Buffer
	if len(req.vars) > 0 {
		variablesField, err := writer.CreateFormField("variables")
		if err != nil {
			return errors.Wrap(err, "failed to create variables field")
		}
		if err := json.NewEncoder(io.MultiWriter(variablesField, &variablesBuf)).Encode(req.vars); err != nil {
			return errors.Wrap(err, "failed to encode variables")
		}
	}

	// Add the files to the multipart request
	for _, file := range req.files {
		part, err := writer.CreateFormFile(file.Field, file.Name)
		if err != nil {
			return errors.Wrap(err, "failed to create form file")
		}
		if _, err := io.Copy(part, file.R); err != nil {
			return errors.Wrap(err, "failed to copy file content")
		}
	}

	// Close the multipart writer to finalize the request body
	if err := writer.Close(); err != nil {
		return errors.Wrap(err, "failed to close writer")
	}

	// Log the request details
	c.logf(">> variables: %s", variablesBuf.String())
	c.logf(">> files: %d", len(req.files))
	c.logf(">> query: %s", req.q)

	// Set the request body and content type
	req.body = requestBody
	req.contentType = writer.FormDataContentType()

	// Make the HTTP request
	return c.makeRequest(ctx, req, resp)
}

func (c *Client) runMultipartRequestSpec(ctx context.Context, req *Request, resp interface{}) error {

	// Ensure no variables are provided as they are not supported for multipart requests
	if len(req.vars) > 0 {
		return errors.New("variables not supported due to the multipart request spec https://github.com/jaydenseric/graphql-multipart-request-spec/issues/22")
	}

	var requestBody bytes.Buffer
	writer := multipart.NewWriter(&requestBody)

	// Prepare the operations and map fields for the multipart request
	multipartRequestSpecQuery := req.fillMultipartRequestSpecQuery()
	operations, err := json.Marshal(multipartRequestSpecQuery.Operations)
	if err != nil {
		return errors.Wrap(err, "failed to marshal operations")
	}

	maps, err := json.Marshal(multipartRequestSpecQuery.Map)
	if err != nil {
		return errors.Wrap(err, "failed to marshal map")
	}

	// Write the operations field
	if err := writer.WriteField("operations", string(operations)); err != nil {
		return errors.Wrap(err, "failed to write operations field")
	}
	c.logf(">> field: %s = %s", "operations", string(operations))

	// Write the map field
	if err := writer.WriteField("map", string(maps)); err != nil {
		return errors.Wrap(err, "failed to write map field")
	}
	c.logf(">> field: %s = %s", "map", string(maps))

	// Add the files to the multipart request
	for _, file := range req.files {
		part, err := writer.CreateFormFile(file.Field, file.Name)
		if err != nil {
			return errors.Wrap(err, "failed to create form file")
		}
		if _, err := io.Copy(part, file.R); err != nil {
			return errors.Wrap(err, "failed to copy file content")
		}
		c.logf(">> file: %s = %s", file.Field, file.Name)
	}

	// Close the multipart writer to finalize the request body
	if err := writer.Close(); err != nil {
		return errors.Wrap(err, "failed to close writer")
	}

	// Set the request body and content type
	req.body = requestBody
	req.contentType = writer.FormDataContentType()

	// Make the HTTP request
	return c.makeRequest(ctx, req, resp)
}

func (c *Client) makeRequest(ctx context.Context, req *Request, resp interface{}) error {
	gr := &graphResponse{
		Data: resp,
	}

	// Create the HTTP request
	r, err := http.NewRequest(http.MethodPost, c.endpoint, &req.body)
	if err != nil {
		return err
	}
	r.Close = c.closeReq
	r.Header.Set("Content-Type", req.contentType)
	r.Header.Set("Accept", "application/json; charset=utf-8")

	// Set additional headers from the request
	for key, values := range req.Header {
		for _, value := range values {
			r.Header.Add(key, value)
		}
	}

	// Log the request headers
	c.logf(">> headers: %v", r.Header)

	// Attach context to the request
	r = r.WithContext(ctx)

	// Send the request
	res, err := c.httpClient.Do(r)
	if err != nil {
		return err
	}
	defer res.Body.Close()

	// Read the response body
	var buf bytes.Buffer
	if _, err := io.Copy(&buf, res.Body); err != nil {
		return errors.Wrap(err, "failed to read response body")
	}

	// Log the response body
	c.logf("<< %s", buf.String())

	// Decode the response into graphResponse
	if err := json.NewDecoder(&buf).Decode(&gr); err != nil {
		if res.StatusCode != http.StatusOK {
			return fmt.Errorf("graphql: server returned a non-200 status code: %v", res.StatusCode)
		}
		return errors.Wrap(err, "failed to decode response")
	}

	// Return the first error if any
	if len(gr.Errors) > 0 {
		return gr.Errors[0]
	}

	return nil
}

type multipartRequestSpecQuery struct {
	Operations struct {
		Query     string      `json:"query"`
		Variables interface{} `json:"variables"`
	} `json:"operations"`
	Map map[string][]string `json:"map"`
}

func (req *Request) fillMultipartRequestSpecQuery() multipartRequestSpecQuery {
	// Define the structures for variables
	type Variables struct {
		Files []interface{} `json:"files"`
	}
	type VariablesEmpty struct{}

	query := new(multipartRequestSpecQuery)
	variables := new(Variables)

	// Set the query in the operations
	query.Operations.Query = req.Query()
	query.Map = make(map[string][]string)

	// Populate the map with file fields and their corresponding variable paths
	for index, file := range req.Files() {
		variables.Files = append(variables.Files, nil)
		query.Map[file.Field] = []string{`variables.files.` + strconv.Itoa(index)}
	}

	// Set the variables in the operations
	if len(req.Files()) > 0 {
		query.Operations.Variables = variables
	} else {
		query.Operations.Variables = new(VariablesEmpty)
	}

	return *query
}

// WithHTTPClient specifies the underlying http.Client to use when
// making requests.
//
//	NewClient(endpoint, WithHTTPClient(specificHTTPClient))
func WithHTTPClient(httpclient *http.Client) ClientOption {
	return func(client *Client) {
		client.httpClient = httpclient
	}
}

// UseMultipartForm uses multipart/form-data and activates support for
// files.
func UseMultipartForm() ClientOption {
	return func(client *Client) {
		client.useMultipartForm = true
	}
}

// UseMultipartRequestSpec uses for files upload, implementing multipart request specification:
// https://github.com/jaydenseric/graphql-multipart-request-spec
// Variables doesn't supported: https://github.com/jaydenseric/graphql-multipart-request-spec/issues/22
func UseMultipartRequestSpec() ClientOption {
	return func(client *Client) {
		client.useMultipartRequestSpec = true
	}
}

// ImmediatelyCloseReqBody will close the req body immediately after each request body is ready
func ImmediatelyCloseReqBody() ClientOption {
	return func(client *Client) {
		client.closeReq = true
	}
}

// ClientOption are functions that are passed into NewClient to
// modify the behaviour of the Client.
type ClientOption func(*Client)

type graphErr struct {
	Message string
}

func (e graphErr) Error() string {
	return "graphql: " + e.Message
}

type graphResponse struct {
	Data   interface{}
	Errors []graphErr
}

// Request is a GraphQL request.
type Request struct {
	q     string
	vars  map[string]interface{}
	files []File

	// Header represent any request headers that will be set
	// when the request is made.
	Header http.Header

	body        bytes.Buffer
	contentType string
}

// NewRequest makes a new Request with the specified string.
func NewRequest(q string) *Request {
	req := &Request{
		q:      q,
		Header: make(map[string][]string),
	}
	return req
}

// Var sets a variable.
func (req *Request) Var(key string, value interface{}) {
	if req.vars == nil {
		req.vars = make(map[string]interface{})
	}
	req.vars[key] = value
}

// Vars gets the variables for this Request.
func (req *Request) Vars() map[string]interface{} {
	return req.vars
}

// Files gets the files in this request.
func (req *Request) Files() []File {
	return req.files
}

// Query gets the query string of this request.
func (req *Request) Query() string {
	return req.q
}

// File sets a file to upload.
// Files are only supported with a Client that was created with
// the UseMultipartForm option.
func (req *Request) File(fieldname, filename string, r io.Reader) {
	req.files = append(req.files, File{
		Field: fieldname,
		Name:  filename,
		R:     r,
	})
}

// File represents a file to upload.
type File struct {
	Field string
	Name  string
	R     io.Reader
}
