// Copyright 2012 Aaron Jacobs. All Rights Reserved.
// Author: aaronjjacobs@gmail.com (Aaron Jacobs)
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

package s3

import (
	"bytes"
	"crypto/md5"
	"encoding/base64"
	"encoding/xml"
	"fmt"
	"github.com/jacobsa/aws"
	"github.com/jacobsa/aws/s3/auth"
	"github.com/jacobsa/aws/s3/http"
	"github.com/jacobsa/aws/time"
	"io"
	sys_http "net/http"
	"net/url"
	sys_time "time"
	"unicode/utf8"
)

// Bucket represents an S3 bucket, which is a collection of objects keyed on
// Unicode strings.
//
// Keys must be non-empty sequences of Unicode characters whose UTF-8 encoding
// is no more than 1024 bytes long. Because S3 returns "list bucket" responses
// as XML 1.0 documents, keys must contain no character that is not a legal
// character as defined by Section 2.2 of the XML 1.0 spec.
//
// See here for more info:
//
//     http://goo.gl/Nd63t
//     http://goo.gl/csem8
//
type Bucket interface {
	// Retrieve data for the object with the given key.
	GetObject(key string) (data []byte, err error)

	// Retrieve headr information for the object with the given key.
	GetHeader(key string) (header sys_http.Header, err error)

	// Store the supplied data with the given key, overwriting any previous
	// version. The object is created with the default ACL of "private".
	StoreObject(key string, data []byte) error

	// Delete the object with the supplied key.
	DeleteObject(key string) error

	// Stream the supplied data with the given key, overwriting any previous
	// version. The object is created with the default ACL of "private".
	Put(key string, data io.ReadSeeker) error

	// Return an ordered set of contiguous object keys in the bucket that are
	// strictly greater than prevKey (or at the beginning of the range if prevKey
	// is empty). It is guaranteed that as some time during the request there
	// were no keys greater than prevKey and less than the first key returned.
	//
	// There may be more keys beyond the last key returned. If no keys are
	// returned (and the error is nil), it is guaranteed that at some time during
	// the request there were the bucket contained no keys in (prevKey, inf).
	//
	// Using this interface you may list all keys in a bucket by starting with
	// the empty string for prevKey (since the empty string is not itself a legal
	// key) and then repeatedly calling again with the last key returned by the
	// previous call.
	//
	// prevKey must be a valid key, with the sole exception that it is allowed to
	// be the empty string.
	ListKeys(prevKey string) (keys []string, err error)
}

// OpenBucket returns a Bucket tied to a given name in a given region. You must
// have previously created the bucket in the region, and the supplied access
// key must have access to it.
//
// To easily create a bucket, use the AWS Console:
//
//     https://console.aws.amazon.com/s3/
//
func OpenBucket(name string, region Region, key aws.AccessKey) (Bucket, error) {
	// Create a connection to the given region's endpoint.
	endpoint := &url.URL{Scheme: "https", Host: string(region)}
	httpConn, err := http.NewConn(endpoint)
	if err != nil {
		return nil, fmt.Errorf("http.NewConn: %v", err)
	}

	// Create an appropriate request signer.
	signer, err := auth.NewSigner(&key)
	if err != nil {
		return nil, fmt.Errorf("auth.NewSigner: %v", err)
	}

	return openBucket(name, httpConn, signer, time.RealClock())
}

// A version of OpenBucket with the ability to inject dependencies, for
// testability.
func openBucket(
	name string,
	httpConn http.Conn,
	signer auth.Signer,
	clock time.Clock) (Bucket, error) {
	return &bucket{name, httpConn, signer, clock}, nil
}

type bucket struct {
	name     string
	httpConn http.Conn
	signer   auth.Signer
	clock    time.Clock
}

////////////////////////////////////////////////////////////////////////
// Common
////////////////////////////////////////////////////////////////////////

func isLegalXmlCharacter(r rune) (inrange bool) {
	return r == 0x09 ||
		r == 0x0A ||
		r == 0x0D ||
		(r >= 0x20 && r <= 0xDF77) ||
		(r >= 0xE000 && r <= 0xFFFD) ||
		(r >= 0x10000 && r <= 0x10FFFF)
}

func validateKey(key string) error {
	// Keys must be valid UTF-8 and no more than 1024 bytes long.
	if len(key) > 1024 {
		return fmt.Errorf("Keys may be no longer than 1024 bytes.")
	}

	if !utf8.ValidString(key) {
		return fmt.Errorf("Keys must be valid UTF-8.")
	}

	// Because of the semantics of the "LIST bucket" request, keys must be
	// non-empty. (Otherwise an empty marker would exclude the first key.) Amazon
	// will reject empty keys with an HTTP 400 response.
	if key == "" {
		return fmt.Errorf("Keys must be non-empty.")
	}

	// Because "LIST bucket" responses are expressed using XML 1.0, keys must
	// contain only characters valid in XML 1.0.
	for _, r := range key {
		if !isLegalXmlCharacter(r) {
			return fmt.Errorf("Key contains invalid codepoint: %U", r)
		}
	}

	return nil
}

func encodeMD5(body io.ReadSeeker) (result string, err error) {
	// start at the begining
	_, err = body.Seek(0, 0)
	if err != nil {
		return
	}

	// make sure we rewind
	defer func() {
		_, err = body.Seek(0, 0)
	}()

	md5Hash := md5.New()

	if _, err := io.Copy(md5Hash, body); err != nil {
		return "", fmt.Errorf("md5Hash.Copy: %v", err)
	}

	base64Md5Buf := new(bytes.Buffer)
	base64Encoder := base64.NewEncoder(base64.StdEncoding, base64Md5Buf)

	if _, err := base64Encoder.Write(md5Hash.Sum(nil)); err != nil {
		return "", fmt.Errorf("base64Encoder.Write: %v", err)
	}
	base64Encoder.Close()

	result = base64Md5Buf.String()
	return
}

func addMd5Header(r *http.Request, body []byte) error {
	bodyMD5, err := encodeMD5(bytes.NewReader(body))
	if err != nil {
		return err
	}

	r.Headers["Content-MD5"] = bodyMD5
	return nil
}

func serverError(httpResp *http.Response) (err error) {
	body, readErr := httpResp.ReadBody()
	if readErr != nil {
		return readErr
	}
	return fmt.Errorf("Error from server: %d %s", httpResp.StatusCode, body)
}

////////////////////////////////////////////////////////////////////////
// GetObject
////////////////////////////////////////////////////////////////////////

func (b *bucket) GetObject(key string) (data []byte, err error) {
	// Validate the key.
	if err := validateKey(key); err != nil {
		return nil, err
	}

	// Build an appropriate HTTP request.
	//
	// Reference:
	//     http://docs.amazonwebservices.com/AmazonS3/latest/API/RESTObjectGET.html
	httpReq := &http.Request{
		Verb: "GET",
		Path: fmt.Sprintf("/%s/%s", b.name, key),
		Headers: map[string]string{
			"Date": b.clock.Now().UTC().Format(sys_time.RFC1123),
		},
	}

	// Sign the request.
	if err := b.signer.Sign(httpReq); err != nil {
		return nil, fmt.Errorf("Sign: %v", err)
	}

	// Send the request.
	httpResp, err := b.httpConn.SendRequest(httpReq)
	if err != nil {
		return nil, fmt.Errorf("SendRequest: %v", err)
	}

	// Check the response.
	if httpResp.StatusCode != 200 {
		return nil, serverError(httpResp)
	}

	data, err = httpResp.ReadBody()
	if err != nil {
		return
	}

	return
}

////////////////////////////////////////////////////////////////////////
// GetHeader
////////////////////////////////////////////////////////////////////////

func (b *bucket) GetHeader(key string) (header sys_http.Header, err error) {
	// Validate the key.
	if err := validateKey(key); err != nil {
		return nil, err
	}

	// Build an appropriate HTTP request.
	//
	// Reference:
	//     http://docs.amazonwebservices.com/AmazonS3/latest/API/RESTObjectGET.html
	httpReq := &http.Request{
		Verb: "HEAD",
		Path: fmt.Sprintf("/%s/%s", b.name, key),
		Headers: map[string]string{
			"Date": b.clock.Now().UTC().Format(sys_time.RFC1123),
		},
	}

	// Sign the request.
	if err := b.signer.Sign(httpReq); err != nil {
		return nil, fmt.Errorf("Sign: %v", err)
	}

	// Send the request.
	httpResp, err := b.httpConn.SendRequest(httpReq)
	if err != nil {
		return nil, fmt.Errorf("SendRequest: %v", err)
	}

	// Check the response.
	if httpResp.StatusCode != 200 {
		return httpResp.Header, serverError(httpResp)
	}

	return httpResp.Header, nil
}

////////////////////////////////////////////////////////////////////////
// StoreObject
////////////////////////////////////////////////////////////////////////

func (b *bucket) StoreObject(key string, data []byte) error {
	// Validate the key.
	if err := validateKey(key); err != nil {
		return err
	}

	// Build an appropriate HTTP request.
	//
	// Reference:
	//     http://docs.amazonwebservices.com/AmazonS3/latest/API/RESTObjectPUT.html
	httpReq := &http.Request{
		Verb: "PUT",
		Path: fmt.Sprintf("/%s/%s", b.name, key),
		Headers: map[string]string{
			"Date": b.clock.Now().UTC().Format(sys_time.RFC1123),
		},
		Body: bytes.NewBuffer(data),
	}

	// Add a Content-MD5 header, as advised in the Amazon docs.
	if err := addMd5Header(httpReq, data); err != nil {
		return err
	}

	// Sign the request.
	if err := b.signer.Sign(httpReq); err != nil {
		return fmt.Errorf("Sign: %v", err)
	}

	// Send the request.
	httpResp, err := b.httpConn.SendRequest(httpReq)
	if err != nil {
		return fmt.Errorf("SendRequest: %v", err)
	}

	// Check the response.
	if httpResp.StatusCode != 200 {
		return serverError(httpResp)
	}

	return nil
}

////////////////////////////////////////////////////////////////////////
// DeleteObject
////////////////////////////////////////////////////////////////////////

func (b *bucket) DeleteObject(key string) error {
	// Validate the key.
	if err := validateKey(key); err != nil {
		return err
	}

	// Build an appropriate HTTP request.
	//
	// Reference:
	//     http://docs.amazonwebservices.com/AmazonS3/latest/API/RESTObjectDELETE.html
	httpReq := &http.Request{
		Verb: "DELETE",
		Path: fmt.Sprintf("/%s/%s", b.name, key),
		Headers: map[string]string{
			"Date": b.clock.Now().UTC().Format(sys_time.RFC1123),
		},
	}

	// Add a Content-MD5 header, as advised in the Amazon docs.
	if err := addMd5Header(httpReq, []byte{}); err != nil {
		return err
	}

	// Sign the request.
	if err := b.signer.Sign(httpReq); err != nil {
		return fmt.Errorf("Sign: %v", err)
	}

	// Send the request.
	httpResp, err := b.httpConn.SendRequest(httpReq)
	if err != nil {
		return fmt.Errorf("SendRequest: %v", err)
	}

	// Check the response.
	if httpResp.StatusCode != 204 {
		return serverError(httpResp)
	}

	return nil
}

////////////////////////////////////////////////////////////////////////
// StoreObject
////////////////////////////////////////////////////////////////////////

func (b *bucket) Put(key string, data io.ReadSeeker) error {
	// Validate the key.
	err := validateKey(key)
	if err != nil {
		return err
	}

	// calculate md5 hash for server verification, as advised in the Amazon docs.
	contentMD5, err := encodeMD5(data)
	if err != nil {
		return err
	}

	// Build an appropriate HTTP request.
	//
	// Reference:
	//     http://docs.amazonwebservices.com/AmazonS3/latest/API/RESTObjectPUT.html
	httpReq := &http.Request{
		Verb: "PUT",
		Path: fmt.Sprintf("/%s/%s", b.name, key),
		Headers: map[string]string{
			"Date":        b.clock.Now().UTC().Format(sys_time.RFC1123),
			"Content-MD5": contentMD5,
		},
		Body: data,
	}

	// Sign the request.
	if err := b.signer.Sign(httpReq); err != nil {
		return fmt.Errorf("Sign: %v", err)
	}

	// Send the request.
	httpResp, err := b.httpConn.SendRequest(httpReq)
	if err != nil {
		return fmt.Errorf("SendRequest: %v", err)
	}

	// Check the response.
	if httpResp.StatusCode != 200 {
		return serverError(httpResp)
	}

	return nil
}

////////////////////////////////////////////////////////////////////////
// ListKeys
////////////////////////////////////////////////////////////////////////

type bucketContents struct {
	Key string
}

type listBucketResult struct {
	XMLName  xml.Name
	Contents []bucketContents
}

func (b *bucket) ListKeys(prevKey string) (keys []string, err error) {
	// Make sure the previous key is empty or valid.
	if err := validateKey(prevKey); err != nil && prevKey != "" {
		return nil, err
	}

	// Build an appropriate HTTP request.
	//
	// Reference:
	//     http://docs.amazonwebservices.com/AmazonS3/latest/API/RESTBucketGET.html
	httpReq := &http.Request{
		Verb: "GET",
		Path: fmt.Sprintf("/%s", b.name),
		Headers: map[string]string{
			"Date": b.clock.Now().UTC().Format(sys_time.RFC1123),
		},
		Parameters: map[string]string{},
	}

	if prevKey != "" {
		httpReq.Parameters["marker"] = prevKey
	}

	// Sign the request.
	if err := b.signer.Sign(httpReq); err != nil {
		return nil, fmt.Errorf("Sign: %v", err)
	}

	// Send the request.
	httpResp, err := b.httpConn.SendRequest(httpReq)
	if err != nil {
		return nil, fmt.Errorf("SendRequest: %v", err)
	}

	// Check the response.
	if httpResp.StatusCode != 200 {
		return nil, serverError(httpResp)
	}

	// Attempt to parse the body.
	body, err := httpResp.ReadBody()
	if err != nil {
		return nil, err
	}

	result := listBucketResult{}
	if err := xml.Unmarshal(body, &result); err != nil {
		return nil, fmt.Errorf(
			"Invalid data from server (%s): %s",
			err.Error(),
			body)
	}

	// Make sure the server agress with us about the interpretation of the
	// request.
	if result.XMLName.Local != "ListBucketResult" {
		return nil, fmt.Errorf("Invalid data from server: %s", httpResp.Body)
	}

	keys = make([]string, len(result.Contents))
	for i, elem := range result.Contents {
		keys[i] = elem.Key
	}

	return keys, nil
}
