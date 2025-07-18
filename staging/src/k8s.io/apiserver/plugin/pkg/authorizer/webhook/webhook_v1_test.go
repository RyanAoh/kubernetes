/*
Copyright 2016 The Kubernetes Authors.

Licensed under the Apache License, Version 2.0 (the "License");
you may not use this file except in compliance with the License.
You may obtain a copy of the License at

    http://www.apache.org/licenses/LICENSE-2.0

Unless required by applicable law or agreed to in writing, software
distributed under the License is distributed on an "AS IS" BASIS,
WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
See the License for the specific language governing permissions and
limitations under the License.
*/

package webhook

import (
	"context"
	"crypto/tls"
	"crypto/x509"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
	"text/template"
	"time"

	"github.com/google/go-cmp/cmp"

	authorizationv1 "k8s.io/api/authorization/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/fields"
	"k8s.io/apimachinery/pkg/labels"
	"k8s.io/apimachinery/pkg/selection"
	"k8s.io/apimachinery/pkg/util/version"
	"k8s.io/apimachinery/pkg/util/wait"
	"k8s.io/apiserver/pkg/apis/apiserver"
	"k8s.io/apiserver/pkg/authentication/user"
	"k8s.io/apiserver/pkg/authorization/authorizer"
	authorizationcel "k8s.io/apiserver/pkg/authorization/cel"
	"k8s.io/apiserver/pkg/features"
	utilfeature "k8s.io/apiserver/pkg/util/feature"
	webhookutil "k8s.io/apiserver/pkg/util/webhook"
	"k8s.io/apiserver/plugin/pkg/authorizer/webhook/metrics"
	v1 "k8s.io/client-go/tools/clientcmd/api/v1"
	utiltesting "k8s.io/client-go/util/testing"
	featuregatetesting "k8s.io/component-base/featuregate/testing"
	"k8s.io/component-base/metrics/legacyregistry"
	"k8s.io/component-base/metrics/testutil"
)

var testRetryBackoff = wait.Backoff{
	Duration: 5 * time.Millisecond,
	Factor:   1.5,
	Jitter:   0.2,
	Steps:    5,
}

func TestV1NewFromConfig(t *testing.T) {
	dir, err := os.MkdirTemp("", "")
	if err != nil {
		t.Fatal(err)
	}
	defer os.RemoveAll(dir)

	data := struct {
		CA   string
		Cert string
		Key  string
	}{
		CA:   filepath.Join(dir, "ca.pem"),
		Cert: filepath.Join(dir, "clientcert.pem"),
		Key:  filepath.Join(dir, "clientkey.pem"),
	}

	files := []struct {
		name string
		data []byte
	}{
		{data.CA, caCert},
		{data.Cert, clientCert},
		{data.Key, clientKey},
	}
	for _, file := range files {
		if err := os.WriteFile(file.name, file.data, 0400); err != nil {
			t.Fatal(err)
		}
	}

	tests := []struct {
		msg        string
		configTmpl string
		wantErr    bool
	}{
		{
			msg: "a single cluster and single user",
			configTmpl: `
clusters:
- cluster:
    certificate-authority: {{ .CA }}
    server: https://authz.example.com
  name: foobar
users:
- name: a cluster
  user:
    client-certificate: {{ .Cert }}
    client-key: {{ .Key }}
`,
			wantErr: true,
		},
		{
			msg: "multiple clusters with no context",
			configTmpl: `
clusters:
- cluster:
    certificate-authority: {{ .CA }}
    server: https://authz.example.com
  name: foobar
- cluster:
    certificate-authority: a bad certificate path
    server: https://authz.example.com
  name: barfoo
users:
- name: a name
  user:
    client-certificate: {{ .Cert }}
    client-key: {{ .Key }}
`,
			wantErr: true,
		},
		{
			msg: "multiple clusters with a context",
			configTmpl: `
clusters:
- cluster:
    certificate-authority: a bad certificate path
    server: https://authz.example.com
  name: foobar
- cluster:
    certificate-authority: {{ .CA }}
    server: https://authz.example.com
  name: barfoo
users:
- name: a name
  user:
    client-certificate: {{ .Cert }}
    client-key: {{ .Key }}
contexts:
- name: default
  context:
    cluster: barfoo
    user: a name
current-context: default
`,
			wantErr: false,
		},
		{
			msg: "cluster with bad certificate path specified",
			configTmpl: `
clusters:
- cluster:
    certificate-authority: a bad certificate path
    server: https://authz.example.com
  name: foobar
- cluster:
    certificate-authority: {{ .CA }}
    server: https://authz.example.com
  name: barfoo
users:
- name: a name
  user:
    client-certificate: {{ .Cert }}
    client-key: {{ .Key }}
contexts:
- name: default
  context:
    cluster: foobar
    user: a name
current-context: default
`,
			wantErr: true,
		},
	}

	for _, tt := range tests {
		// Use a closure so defer statements trigger between loop iterations.
		err := func() error {
			tempfile, err := os.CreateTemp("", "")
			if err != nil {
				return err
			}
			p := tempfile.Name()
			defer utiltesting.CloseAndRemove(t, tempfile)

			tmpl, err := template.New("test").Parse(tt.configTmpl)
			if err != nil {
				return fmt.Errorf("failed to parse test template: %v", err)
			}
			if err := tmpl.Execute(tempfile, data); err != nil {
				return fmt.Errorf("failed to execute test template: %v", err)
			}
			// Create a new authorizer
			clientConfig, err := webhookutil.LoadKubeconfig(p, nil)
			if err != nil {
				return err
			}
			sarClient, err := subjectAccessReviewInterfaceFromConfig(clientConfig, "v1", testRetryBackoff)
			if err != nil {
				return fmt.Errorf("error building sar client: %v", err)
			}
			_, err = newWithBackoff(sarClient, 0, 0, testRetryBackoff, authorizer.DecisionNoOpinion, []apiserver.WebhookMatchCondition{}, noopAuthorizerMetrics(), authorizationcel.NewDefaultCompiler(), "")
			return err
		}()
		if err != nil && !tt.wantErr {
			t.Errorf("failed to load plugin from config %q: %v", tt.msg, err)
		}
		if err == nil && tt.wantErr {
			t.Errorf("wanted an error when loading config, did not get one: %q", tt.msg)
		}
	}
}

// V1Service mocks a remote service.
type V1Service interface {
	Review(*authorizationv1.SubjectAccessReview)
	HTTPStatusCode() int
}

// NewV1TestServer wraps a V1Service as an httptest.Server.
func NewV1TestServer(s V1Service, cert, key, caCert []byte) (*httptest.Server, error) {
	const webhookPath = "/testserver"
	var tlsConfig *tls.Config
	if cert != nil {
		cert, err := tls.X509KeyPair(cert, key)
		if err != nil {
			return nil, err
		}
		tlsConfig = &tls.Config{Certificates: []tls.Certificate{cert}}
	}

	if caCert != nil {
		rootCAs := x509.NewCertPool()
		rootCAs.AppendCertsFromPEM(caCert)
		if tlsConfig == nil {
			tlsConfig = &tls.Config{}
		}
		tlsConfig.ClientCAs = rootCAs
		tlsConfig.ClientAuth = tls.RequireAndVerifyClientCert
	}

	serveHTTP := func(w http.ResponseWriter, r *http.Request) {
		if r.Method != "POST" {
			http.Error(w, fmt.Sprintf("unexpected method: %v", r.Method), http.StatusMethodNotAllowed)
			return
		}
		if r.URL.Path != webhookPath {
			http.Error(w, fmt.Sprintf("unexpected path: %v", r.URL.Path), http.StatusNotFound)
			return
		}

		var review authorizationv1.SubjectAccessReview
		bodyData, _ := io.ReadAll(r.Body)
		if err := json.Unmarshal(bodyData, &review); err != nil {
			http.Error(w, fmt.Sprintf("failed to decode body: %v", err), http.StatusBadRequest)
			return
		}

		// ensure we received the serialized review as expected
		if review.APIVersion != "authorization.k8s.io/v1" {
			http.Error(w, fmt.Sprintf("wrong api version: %s", string(bodyData)), http.StatusBadRequest)
			return
		}
		// once we have a successful request, always call the review to record that we were called
		s.Review(&review)
		if s.HTTPStatusCode() < 200 || s.HTTPStatusCode() >= 300 {
			http.Error(w, "HTTP Error", s.HTTPStatusCode())
			return
		}
		type status struct {
			Allowed         bool   `json:"allowed"`
			Reason          string `json:"reason"`
			EvaluationError string `json:"evaluationError"`
		}
		resp := struct {
			APIVersion string `json:"apiVersion"`
			Status     status `json:"status"`
		}{
			APIVersion: authorizationv1.SchemeGroupVersion.String(),
			Status:     status{review.Status.Allowed, review.Status.Reason, review.Status.EvaluationError},
		}
		w.Header().Set("Content-Type", "application/json")
		json.NewEncoder(w).Encode(resp)
	}

	server := httptest.NewUnstartedServer(http.HandlerFunc(serveHTTP))
	server.TLS = tlsConfig
	server.StartTLS()

	// Adjust the path to point to our custom path
	serverURL, _ := url.Parse(server.URL)
	serverURL.Path = webhookPath
	server.URL = serverURL.String()

	return server, nil
}

// A service that can be set to allow all or deny all authorization requests.
type mockV1Service struct {
	allow      bool
	statusCode int
	called     int

	// reviewHook is called just before returning from the Review() method
	reviewHook func(*authorizationv1.SubjectAccessReview)
}

func (m *mockV1Service) Review(r *authorizationv1.SubjectAccessReview) {
	m.called++
	r.Status.Allowed = m.allow

	if m.reviewHook != nil {
		m.reviewHook(r)
	}
}
func (m *mockV1Service) Allow()              { m.allow = true }
func (m *mockV1Service) Deny()               { m.allow = false }
func (m *mockV1Service) HTTPStatusCode() int { return m.statusCode }

// newV1Authorizer creates a temporary kubeconfig file from the provided arguments and attempts to load
// a new WebhookAuthorizer from it.
func newV1Authorizer(callbackURL string, clientCert, clientKey, ca []byte, cacheTime time.Duration, metrics metrics.AuthorizerMetrics, compiler authorizationcel.Compiler, expressions []apiserver.WebhookMatchCondition, authzName string) (*WebhookAuthorizer, error) {
	tempfile, err := os.CreateTemp("", "")
	if err != nil {
		return nil, err
	}
	p := tempfile.Name()
	defer os.Remove(p)
	config := v1.Config{
		Clusters: []v1.NamedCluster{
			{
				Cluster: v1.Cluster{Server: callbackURL, CertificateAuthorityData: ca},
			},
		},
		AuthInfos: []v1.NamedAuthInfo{
			{
				AuthInfo: v1.AuthInfo{ClientCertificateData: clientCert, ClientKeyData: clientKey},
			},
		},
	}
	if err := json.NewEncoder(tempfile).Encode(config); err != nil {
		return nil, err
	}
	clientConfig, err := webhookutil.LoadKubeconfig(p, nil)
	if err != nil {
		return nil, err
	}
	sarClient, err := subjectAccessReviewInterfaceFromConfig(clientConfig, "v1", testRetryBackoff)
	if err != nil {
		return nil, fmt.Errorf("error building sar client: %v", err)
	}
	return newWithBackoff(sarClient, cacheTime, cacheTime, testRetryBackoff, authorizer.DecisionNoOpinion, expressions, metrics, compiler, authzName)
}

func TestV1TLSConfig(t *testing.T) {
	tests := []struct {
		test                            string
		clientCert, clientKey, clientCA []byte
		serverCert, serverKey, serverCA []byte
		wantAuth, wantErr               bool
	}{
		{
			test:       "TLS setup between client and server",
			clientCert: clientCert, clientKey: clientKey, clientCA: caCert,
			serverCert: serverCert, serverKey: serverKey, serverCA: caCert,
			wantAuth: true,
		},
		{
			test:       "Server does not require client auth",
			clientCA:   caCert,
			serverCert: serverCert, serverKey: serverKey,
			wantAuth: true,
		},
		{
			test:       "Server does not require client auth, client provides it",
			clientCert: clientCert, clientKey: clientKey, clientCA: caCert,
			serverCert: serverCert, serverKey: serverKey,
			wantAuth: true,
		},
		{
			test:       "Client does not trust server",
			clientCert: clientCert, clientKey: clientKey,
			serverCert: serverCert, serverKey: serverKey,
			wantErr: true,
		},
		{
			test:       "Server does not trust client",
			clientCert: clientCert, clientKey: clientKey, clientCA: caCert,
			serverCert: serverCert, serverKey: serverKey, serverCA: badCACert,
			wantErr: true,
		},
		{
			// Plugin does not support insecure configurations.
			test:    "Server is using insecure connection",
			wantErr: true,
		},
	}
	for _, tt := range tests {
		// Use a closure so defer statements trigger between loop iterations.
		func() {
			service := new(mockV1Service)
			service.statusCode = 200

			server, err := NewV1TestServer(service, tt.serverCert, tt.serverKey, tt.serverCA)
			if err != nil {
				t.Errorf("%s: failed to create server: %v", tt.test, err)
				return
			}
			defer server.Close()

			wh, err := newV1Authorizer(server.URL, tt.clientCert, tt.clientKey, tt.clientCA, 0, noopAuthorizerMetrics(), authorizationcel.NewDefaultCompiler(), []apiserver.WebhookMatchCondition{}, "")
			if err != nil {
				t.Errorf("%s: failed to create client: %v", tt.test, err)
				return
			}

			attr := authorizer.AttributesRecord{User: &user.DefaultInfo{}}

			// Allow all and see if we get an error.
			service.Allow()
			decision, _, err := wh.Authorize(context.Background(), attr)
			if tt.wantAuth {
				if decision != authorizer.DecisionAllow {
					t.Errorf("expected successful authorization")
				}
			} else {
				if decision == authorizer.DecisionAllow {
					t.Errorf("expected failed authorization")
				}
			}
			if tt.wantErr {
				if err == nil {
					t.Errorf("expected error making authorization request: %v", err)
				}
				return
			}
			if err != nil {
				t.Errorf("%s: failed to authorize with AllowAll policy: %v", tt.test, err)
				return
			}

			service.Deny()
			if decision, _, _ := wh.Authorize(context.Background(), attr); decision == authorizer.DecisionAllow {
				t.Errorf("%s: incorrectly authorized with DenyAll policy", tt.test)
			}
		}()
	}
}

// recorderV1Service records all access review requests.
type recorderV1Service struct {
	last authorizationv1.SubjectAccessReview
	err  error
}

func (rec *recorderV1Service) Review(r *authorizationv1.SubjectAccessReview) {
	rec.last = authorizationv1.SubjectAccessReview{}
	rec.last = *r
	r.Status.Allowed = true
}

func (rec *recorderV1Service) Last() (authorizationv1.SubjectAccessReview, error) {
	return rec.last, rec.err
}

func (rec *recorderV1Service) HTTPStatusCode() int { return 200 }

func TestV1Webhook(t *testing.T) {
	serv := new(recorderV1Service)
	s, err := NewV1TestServer(serv, serverCert, serverKey, caCert)
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()

	wh, err := newV1Authorizer(s.URL, clientCert, clientKey, caCert, 0, noopAuthorizerMetrics(), authorizationcel.NewDefaultCompiler(), []apiserver.WebhookMatchCondition{}, "")
	if err != nil {
		t.Fatal(err)
	}

	expTypeMeta := metav1.TypeMeta{
		APIVersion: "authorization.k8s.io/v1",
		Kind:       "SubjectAccessReview",
	}

	tests := []struct {
		attr authorizer.Attributes
		want authorizationv1.SubjectAccessReview
	}{
		{
			attr: authorizer.AttributesRecord{User: &user.DefaultInfo{}},
			want: authorizationv1.SubjectAccessReview{
				TypeMeta: expTypeMeta,
				Spec: authorizationv1.SubjectAccessReviewSpec{
					NonResourceAttributes: &authorizationv1.NonResourceAttributes{},
				},
			},
		},
		{
			attr: authorizer.AttributesRecord{User: &user.DefaultInfo{Name: "jane"}},
			want: authorizationv1.SubjectAccessReview{
				TypeMeta: expTypeMeta,
				Spec: authorizationv1.SubjectAccessReviewSpec{
					User:                  "jane",
					NonResourceAttributes: &authorizationv1.NonResourceAttributes{},
				},
			},
		},
		{
			attr: authorizer.AttributesRecord{
				User: &user.DefaultInfo{
					Name:   "jane",
					UID:    "1",
					Groups: []string{"group1", "group2"},
				},
				Verb:            "GET",
				Namespace:       "kittensandponies",
				APIGroup:        "group3",
				APIVersion:      "v7beta3",
				Resource:        "pods",
				Subresource:     "proxy",
				Name:            "my-pod",
				ResourceRequest: true,
				Path:            "/foo",
			},
			want: authorizationv1.SubjectAccessReview{
				TypeMeta: expTypeMeta,
				Spec: authorizationv1.SubjectAccessReviewSpec{
					User:   "jane",
					UID:    "1",
					Groups: []string{"group1", "group2"},
					ResourceAttributes: &authorizationv1.ResourceAttributes{
						Verb:        "GET",
						Namespace:   "kittensandponies",
						Group:       "group3",
						Version:     "v7beta3",
						Resource:    "pods",
						Subresource: "proxy",
						Name:        "my-pod",
					},
				},
			},
		},
	}

	for i, tt := range tests {
		decision, _, err := wh.Authorize(context.Background(), tt.attr)
		if err != nil {
			t.Fatal(err)
		}
		if decision != authorizer.DecisionAllow {
			t.Errorf("case %d: authorization failed", i)
			continue
		}

		gotAttr, err := serv.Last()
		if err != nil {
			t.Errorf("case %d: failed to deserialize webhook request: %v", i, err)
			continue
		}
		if !reflect.DeepEqual(gotAttr, tt.want) {
			t.Errorf("case %d: got != want:\n%s", i, cmp.Diff(gotAttr, tt.want))
		}
	}
}

// TestWebhookCache verifies that error responses from the server are not
// cached, but successful responses are.
func TestV1WebhookCache(t *testing.T) {
	serv := new(mockV1Service)
	s, err := NewV1TestServer(serv, serverCert, serverKey, caCert)
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	expressions := []apiserver.WebhookMatchCondition{
		{
			Expression: "has(request.resourceAttributes) && request.resourceAttributes.namespace == 'kittensandponies'",
		},
	}
	// Create an authorizer that caches successful responses "forever" (100 days).
	wh, err := newV1Authorizer(s.URL, clientCert, clientKey, caCert, 2400*time.Hour, noopAuthorizerMetrics(), authorizationcel.NewDefaultCompiler(), expressions, "")
	if err != nil {
		t.Fatal(err)
	}

	aliceAttr := authorizer.AttributesRecord{User: &user.DefaultInfo{Name: "alice"}, ResourceRequest: true, Namespace: "kittensandponies"}
	bobAttr := authorizer.AttributesRecord{User: &user.DefaultInfo{Name: "bob"}, ResourceRequest: true, Namespace: "kittensandponies"}
	aliceRidiculousAttr := authorizer.AttributesRecord{
		User:            &user.DefaultInfo{Name: "alice"},
		ResourceRequest: true,
		Verb:            strings.Repeat("v", 2000),
		APIGroup:        strings.Repeat("g", 2000),
		APIVersion:      strings.Repeat("a", 2000),
		Resource:        strings.Repeat("r", 2000),
		Name:            strings.Repeat("n", 2000),
		Namespace:       "kittensandponies",
	}
	bobRidiculousAttr := authorizer.AttributesRecord{
		User:            &user.DefaultInfo{Name: "bob"},
		ResourceRequest: true,
		Verb:            strings.Repeat("v", 2000),
		APIGroup:        strings.Repeat("g", 2000),
		APIVersion:      strings.Repeat("a", 2000),
		Resource:        strings.Repeat("r", 2000),
		Name:            strings.Repeat("n", 2000),
		Namespace:       "kittensandponies",
	}

	type webhookCacheTestCase struct {
		name string

		attr authorizer.AttributesRecord

		allow      bool
		statusCode int

		expectedErr        bool
		expectedAuthorized bool
		expectedCalls      int
	}

	tests := []webhookCacheTestCase{
		// server error and 429's retry
		{name: "server errors retry", attr: aliceAttr, allow: false, statusCode: 500, expectedErr: true, expectedAuthorized: false, expectedCalls: 5},
		{name: "429s retry", attr: aliceAttr, allow: false, statusCode: 429, expectedErr: true, expectedAuthorized: false, expectedCalls: 5},
		// regular errors return errors but do not retry
		{name: "404 doesnt retry", attr: aliceAttr, allow: false, statusCode: 404, expectedErr: true, expectedAuthorized: false, expectedCalls: 1},
		{name: "403 doesnt retry", attr: aliceAttr, allow: false, statusCode: 403, expectedErr: true, expectedAuthorized: false, expectedCalls: 1},
		{name: "401 doesnt retry", attr: aliceAttr, allow: false, statusCode: 401, expectedErr: true, expectedAuthorized: false, expectedCalls: 1},
		// successful responses are cached
		{name: "alice successful request", attr: aliceAttr, allow: true, statusCode: 200, expectedErr: false, expectedAuthorized: true, expectedCalls: 1},
		// later requests within the cache window don't hit the backend
		{name: "alice cached request", attr: aliceAttr, allow: false, statusCode: 500, expectedErr: false, expectedAuthorized: true, expectedCalls: 0},

		// a request with different attributes doesn't hit the cache
		{name: "bob failed request", attr: bobAttr, allow: false, statusCode: 500, expectedErr: true, expectedAuthorized: false, expectedCalls: 5},
		// successful response for other attributes is cached
		{name: "bob unauthorized request", attr: bobAttr, allow: false, statusCode: 200, expectedErr: false, expectedAuthorized: false, expectedCalls: 1},
		// later requests within the cache window don't hit the backend
		{name: "bob unauthorized cached request", attr: bobAttr, allow: false, statusCode: 500, expectedErr: false, expectedAuthorized: false, expectedCalls: 0},
		// ridiculous unauthorized requests are not cached.
		{name: "ridiculous unauthorized request", attr: bobRidiculousAttr, allow: false, statusCode: 200, expectedErr: false, expectedAuthorized: false, expectedCalls: 1},
		// later ridiculous requests within the cache window still hit the backend
		{name: "ridiculous unauthorized request again", attr: bobRidiculousAttr, allow: false, statusCode: 200, expectedErr: false, expectedAuthorized: false, expectedCalls: 1},
		// ridiculous authorized requests are not cached.
		{name: "ridiculous authorized request", attr: aliceRidiculousAttr, allow: true, statusCode: 200, expectedErr: false, expectedAuthorized: true, expectedCalls: 1},
		// later ridiculous requests within the cache window still hit the backend
		{name: "ridiculous authorized request again", attr: aliceRidiculousAttr, allow: true, statusCode: 200, expectedErr: false, expectedAuthorized: true, expectedCalls: 1},
	}

	for i, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			serv.called = 0
			serv.allow = test.allow
			serv.statusCode = test.statusCode
			authorized, _, err := wh.Authorize(context.Background(), test.attr)
			if test.expectedErr && err == nil {
				t.Fatalf("%d: Expected error", i)
			} else if !test.expectedErr && err != nil {
				t.Fatalf("%d: unexpected error: %v", i, err)
			}

			if test.expectedAuthorized != (authorized == authorizer.DecisionAllow) {
				t.Errorf("%d: expected authorized=%v, got %v", i, test.expectedAuthorized, authorized)
			}

			if test.expectedCalls != serv.called {
				t.Errorf("%d: expected %d calls, got %d", i, test.expectedCalls, serv.called)
			}
		})
	}
}

// TestStructuredAuthzConfigFeatureEnablement verifies cel expressions can only be used when feature is enabled
func TestStructuredAuthzConfigFeatureEnablement(t *testing.T) {
	service := new(mockV1Service)
	service.statusCode = 200
	service.Allow()
	s, err := NewV1TestServer(service, serverCert, serverKey, caCert)
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()

	labelRequirement, _ := labels.NewRequirement("baz", selection.Equals, []string{"qux"})

	type webhookMatchConditionsTestCase struct {
		name               string
		attr               authorizer.AttributesRecord
		allow              bool
		expectedCompileErr bool
		expectedEvalErr    bool
		expectedDecision   authorizer.Decision
		expressions        []apiserver.WebhookMatchCondition
		selectorEnabled    bool
	}
	aliceAttr := authorizer.AttributesRecord{
		User: &user.DefaultInfo{
			Name:   "alice",
			UID:    "1",
			Groups: []string{"group1", "group2"},
			Extra:  map[string][]string{"key1": {"a", "b", "c"}},
		},
		ResourceRequest: true,
		Namespace:       "kittensandponies",
		Verb:            "get",
	}
	aliceWithSelectorsAttr := authorizer.AttributesRecord{
		User: &user.DefaultInfo{
			Name:   "alice",
			UID:    "1",
			Groups: []string{"group1", "group2"},
			Extra:  map[string][]string{"key1": {"a", "b", "c"}},
		},
		ResourceRequest:           true,
		Namespace:                 "kittensandponies",
		Verb:                      "get",
		FieldSelectorRequirements: fields.Requirements{fields.Requirement{Field: "foo", Operator: selection.Equals, Value: "bar"}},
		LabelSelectorRequirements: labels.Requirements{*labelRequirement},
	}
	tests := []webhookMatchConditionsTestCase{
		{
			name:               "no match condition does not require feature enablement",
			attr:               aliceAttr,
			allow:              true,
			expectedCompileErr: false,
			expectedDecision:   authorizer.DecisionAllow,
			expressions:        []apiserver.WebhookMatchCondition{},
		},
		{
			name:               "feature enabled, match all against all expressions",
			attr:               aliceWithSelectorsAttr,
			allow:              true,
			expectedCompileErr: false,
			expectedDecision:   authorizer.DecisionAllow,
			expressions: []apiserver.WebhookMatchCondition{
				{
					Expression: "request.user == 'alice'",
				},
				{
					Expression: "request.uid == '1'",
				},
				{
					Expression: "('group1' in request.groups)",
				},
				{
					Expression: "has(request.resourceAttributes) && request.resourceAttributes.namespace == 'kittensandponies'",
				},
				{
					Expression: "request.?resourceAttributes.fieldSelector.requirements.orValue([]).exists(r, r.key=='foo' && r.operator=='In' && ('bar' in r.values))",
				},
				{
					Expression: "request.?resourceAttributes.labelSelector.requirements.orValue([]).exists(r, r.key=='baz' && r.operator=='In' && ('qux' in r.values))",
				},
				{
					Expression: "request.resourceAttributes.?labelSelector.requirements.orValue([]).exists(r, r.key=='baz' && r.operator=='In' && ('qux' in r.values))",
				},
				{
					Expression: "request.resourceAttributes.labelSelector.?requirements.orValue([]).exists(r, r.key=='baz' && r.operator=='In' && ('qux' in r.values))",
				},
			},
			selectorEnabled: true,
		},
	}

	for i, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			if !test.selectorEnabled {
				featuregatetesting.SetFeatureGateEmulationVersionDuringTest(t, utilfeature.DefaultFeatureGate, version.MustParse("1.33"))
				featuregatetesting.SetFeatureGateDuringTest(t, utilfeature.DefaultFeatureGate, features.AuthorizeWithSelectors, false)
			}

			// create new compiler because it depends on the feature gate
			compiler := authorizationcel.NewDefaultCompiler()

			wh, err := newV1Authorizer(s.URL, clientCert, clientKey, caCert, 0, noopAuthorizerMetrics(), compiler, test.expressions, "")
			if test.expectedCompileErr && err == nil {
				t.Fatalf("%d: Expected compile error", i)
			} else if !test.expectedCompileErr && err != nil {
				t.Fatalf("%d: unexpected error when creating a new WebhookAuthorizer: %v", i, err)
			}
			if err == nil {
				authorized, _, err := wh.Authorize(context.Background(), test.attr)
				if test.expectedEvalErr && err == nil {
					t.Fatalf("%d: Expected eval error", i)
				} else if !test.expectedEvalErr && err != nil {
					t.Fatalf("%d: unexpected error when authorizing: %v", i, err)
				}

				if test.expectedDecision != authorized {
					t.Errorf("%d: expected authorized=%v, got %v", i, test.expectedDecision, authorized)
				}
			}
		})
	}
}

func TestWebhookMetrics(t *testing.T) {
	service := new(mockV1Service)
	service.statusCode = 200
	service.Allow()
	s, err := NewV1TestServer(service, serverCert, serverKey, caCert)
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()

	aliceAttr := authorizer.AttributesRecord{
		User: &user.DefaultInfo{
			Name: "alice",
			UID:  "1",
		},
	}

	testCases := []struct {
		name         string
		attr         authorizer.AttributesRecord
		expressions1 []apiserver.WebhookMatchCondition
		expressions2 []apiserver.WebhookMatchCondition
		metrics      []string
		want         string
	}{
		{
			name: "should have one evaluation error from multiple failed match conditions",
			attr: aliceAttr,
			expressions1: []apiserver.WebhookMatchCondition{
				{
					Expression: "request.user == 'alice'",
				},
				{
					Expression: "request.resourceAttributes.verb == 'get'",
				},
				{
					Expression: "request.resourceAttributes.namespace == 'kittensandponies'",
				},
			},
			expressions2: []apiserver.WebhookMatchCondition{
				{
					Expression: "request.user == 'alice'",
				},
			},
			metrics: []string{
				"apiserver_authorization_match_condition_evaluation_errors_total",
			},
			want: fmt.Sprintf(`
					# HELP apiserver_authorization_match_condition_evaluation_errors_total [ALPHA] Total number of errors when an authorization webhook encounters a match condition error split by authorizer type and name.
					# TYPE apiserver_authorization_match_condition_evaluation_errors_total counter
					apiserver_authorization_match_condition_evaluation_errors_total{name="%s",type="%s"} 1
					`, "wh1.example.com", "Webhook"),
		},
		{
			name: "should have two webhook exclusions due to match condition",
			attr: aliceAttr,
			expressions1: []apiserver.WebhookMatchCondition{
				{
					Expression: "request.user == 'alice2'",
				},
				{
					Expression: "request.uid == '1'",
				},
			},
			expressions2: []apiserver.WebhookMatchCondition{
				{
					Expression: "request.user == 'alice1'",
				},
			},
			metrics: []string{
				"apiserver_authorization_match_condition_exclusions_total",
			},
			want: fmt.Sprintf(`
					# HELP apiserver_authorization_match_condition_exclusions_total [ALPHA] Total number of exclusions when an authorization webhook is skipped because match conditions exclude it.
					# TYPE apiserver_authorization_match_condition_exclusions_total counter
					apiserver_authorization_match_condition_exclusions_total{name="%s",type="%s"} 1
					apiserver_authorization_match_condition_exclusions_total{name="%s",type="%s"} 1
					`, "wh1.example.com", "Webhook", "wh2.example.com", "Webhook"),
		},
	}

	for _, tt := range testCases {
		t.Run(tt.name, func(t *testing.T) {
			authorizationcel.ResetMetricsForTest()
			defer authorizationcel.ResetMetricsForTest()
			wh1, err := newV1Authorizer(s.URL, clientCert, clientKey, caCert, 0, celAuthorizerMetrics(), authorizationcel.NewDefaultCompiler(), tt.expressions1, "wh1.example.com")
			if err != nil {
				t.Fatal(err)
			}
			wh2, err := newV1Authorizer(s.URL, clientCert, clientKey, caCert, 0, celAuthorizerMetrics(), authorizationcel.NewDefaultCompiler(), tt.expressions2, "wh2.example.com")
			if err != nil {
				t.Fatal(err)
			}
			if err == nil {
				_, _, _ = wh1.Authorize(context.Background(), tt.attr)
				_, _, _ = wh2.Authorize(context.Background(), tt.attr)
			}

			if err := testutil.GatherAndCompare(legacyregistry.DefaultGatherer, strings.NewReader(tt.want), tt.metrics...); err != nil {
				t.Fatal(err)
			}
		})
	}
}

func BenchmarkNoCELExpressionFeatureOn(b *testing.B) {
	expressions := []apiserver.WebhookMatchCondition{}
	b.Run("compile", func(b *testing.B) {
		benchmarkNewWebhookAuthorizer(b, expressions)
	})
	b.Run("authorize", func(b *testing.B) {
		benchmarkWebhookAuthorize(b, expressions)
	})
}
func BenchmarkWithOneCELExpressions(b *testing.B) {
	expressions := []apiserver.WebhookMatchCondition{
		{
			Expression: "request.user == 'alice'",
		},
	}
	b.Run("compile", func(b *testing.B) {
		benchmarkNewWebhookAuthorizer(b, expressions)
	})
	b.Run("authorize", func(b *testing.B) {
		benchmarkWebhookAuthorize(b, expressions)
	})
}
func BenchmarkWithOneCELExpressionsFalse(b *testing.B) {
	expressions := []apiserver.WebhookMatchCondition{
		{
			Expression: "request.user == 'alice2'",
		},
	}
	b.Run("compile", func(b *testing.B) {
		benchmarkNewWebhookAuthorizer(b, expressions)
	})
	b.Run("authorize", func(b *testing.B) {
		benchmarkWebhookAuthorize(b, expressions)
	})
}
func BenchmarkWithTwoCELExpressions(b *testing.B) {
	expressions := []apiserver.WebhookMatchCondition{
		{
			Expression: "request.user == 'alice'",
		},
		{
			Expression: "request.uid == '1'",
		},
	}
	b.Run("compile", func(b *testing.B) {
		benchmarkNewWebhookAuthorizer(b, expressions)
	})
	b.Run("authorize", func(b *testing.B) {
		benchmarkWebhookAuthorize(b, expressions)
	})
}
func BenchmarkWithTwoCELExpressionsFalse(b *testing.B) {
	expressions := []apiserver.WebhookMatchCondition{
		{
			Expression: "request.user == 'alice'",
		},
		{
			Expression: "request.uid == '2'",
		},
	}
	b.Run("compile", func(b *testing.B) {
		benchmarkNewWebhookAuthorizer(b, expressions)
	})
	b.Run("authorize", func(b *testing.B) {
		benchmarkWebhookAuthorize(b, expressions)
	})
}
func BenchmarkWithManyCELExpressions(b *testing.B) {
	expressions := []apiserver.WebhookMatchCondition{
		{
			Expression: "request.user == 'alice'",
		},
		{
			Expression: "request.uid == '1'",
		},
		{
			Expression: "('group1' in request.groups)",
		},
		{
			Expression: "('key1' in request.extra)",
		},
		{
			Expression: "!('key2' in request.extra)",
		},
		{
			Expression: "('a' in request.extra['key1'])",
		},
		{
			Expression: "!('z' in request.extra['key1'])",
		},
		{
			Expression: "has(request.resourceAttributes) && request.resourceAttributes.namespace == 'kittensandponies'",
		},
	}
	b.Run("compile", func(b *testing.B) {
		benchmarkNewWebhookAuthorizer(b, expressions)
	})
	b.Run("authorize", func(b *testing.B) {
		benchmarkWebhookAuthorize(b, expressions)
	})
}
func BenchmarkWithManyCELExpressionsFalse(b *testing.B) {
	expressions := []apiserver.WebhookMatchCondition{
		{
			Expression: "request.user == 'alice'",
		},
		{
			Expression: "request.uid == '1'",
		},
		{
			Expression: "('group1' in request.groups)",
		},
		{
			Expression: "('key1' in request.extra)",
		},
		{
			Expression: "!('key2' in request.extra)",
		},
		{
			Expression: "('a' in request.extra['key1'])",
		},
		{
			Expression: "!('z' in request.extra['key1'])",
		},
		{
			Expression: "has(request.resourceAttributes) && request.resourceAttributes.namespace == 'kittensandponies1'",
		},
	}
	b.Run("compile", func(b *testing.B) {
		benchmarkNewWebhookAuthorizer(b, expressions)
	})
	b.Run("authorize", func(b *testing.B) {
		benchmarkWebhookAuthorize(b, expressions)
	})
}

func benchmarkNewWebhookAuthorizer(b *testing.B, expressions []apiserver.WebhookMatchCondition) {
	service := new(mockV1Service)
	service.statusCode = 200
	service.Allow()
	s, err := NewV1TestServer(service, serverCert, serverKey, caCert)
	if err != nil {
		b.Fatal(err)
	}
	defer s.Close()

	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		// Create an authorizer with or without expressions to compile
		_, err := newV1Authorizer(s.URL, clientCert, clientKey, caCert, 0, noopAuthorizerMetrics(), authorizationcel.NewDefaultCompiler(), expressions, "")
		if err != nil {
			b.Fatal(err)
		}
	}
	b.StopTimer()
}

func benchmarkWebhookAuthorize(b *testing.B, expressions []apiserver.WebhookMatchCondition) {
	attr := authorizer.AttributesRecord{
		User: &user.DefaultInfo{
			Name:   "alice",
			UID:    "1",
			Groups: []string{"group1", "group2"},
			Extra:  map[string][]string{"key1": {"a", "b", "c"}},
		},
		ResourceRequest: true,
		Namespace:       "kittensandponies",
		Verb:            "get",
	}
	service := new(mockV1Service)
	service.statusCode = 200
	service.Allow()
	s, err := NewV1TestServer(service, serverCert, serverKey, caCert)
	if err != nil {
		b.Fatal(err)
	}
	defer s.Close()
	// Create an authorizer with or without expressions to compile
	wh, err := newV1Authorizer(s.URL, clientCert, clientKey, caCert, 0, noopAuthorizerMetrics(), authorizationcel.NewDefaultCompiler(), expressions, "")
	if err != nil {
		b.Fatal(err)
	}

	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		// Call authorize may or may not require cel evaluations
		_, _, err = wh.Authorize(context.Background(), attr)
		if err != nil {
			b.Fatal(err)
		}
	}
	b.StopTimer()
}

// TestV1WebhookMatchConditions verifies cel expressions are compiled and evaluated correctly
func TestV1WebhookMatchConditions(t *testing.T) {
	service := new(mockV1Service)
	service.statusCode = 200
	service.Allow()
	s, err := NewV1TestServer(service, serverCert, serverKey, caCert)
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()

	aliceAttr := authorizer.AttributesRecord{
		User: &user.DefaultInfo{
			Name:   "alice",
			UID:    "1",
			Groups: []string{"group1", "group2"},
			Extra:  map[string][]string{"key1": {"a", "b", "c"}},
		},
		ResourceRequest: true,
		Namespace:       "kittensandponies",
		Verb:            "get",
	}
	bobAttr := authorizer.AttributesRecord{
		User: &user.DefaultInfo{
			Name: "bob",
		},
		ResourceRequest: false,
		Namespace:       "kittensandponies",
		Verb:            "get",
	}
	alice2Attr := authorizer.AttributesRecord{
		User: &user.DefaultInfo{
			Name: "alice2",
		},
	}
	type webhookMatchConditionsTestCase struct {
		name               string
		attr               authorizer.AttributesRecord
		expectedCompileErr string
		expectedEvalErr    string
		expectedDecision   authorizer.Decision
		expressions        []apiserver.WebhookMatchCondition
	}

	tests := []webhookMatchConditionsTestCase{
		{
			name:               "match all with no expressions",
			attr:               aliceAttr,
			expectedCompileErr: "",
			expectedDecision:   authorizer.DecisionAllow,
			expressions:        []apiserver.WebhookMatchCondition{},
		},
		{
			name:               "match all against all expressions",
			attr:               aliceAttr,
			expectedCompileErr: "",
			expectedDecision:   authorizer.DecisionAllow,
			expressions: []apiserver.WebhookMatchCondition{
				{
					Expression: "request.user == 'alice'",
				},
				{
					Expression: "request.uid == '1'",
				},
				{
					Expression: "('group1' in request.groups)",
				},
				{
					Expression: "('key1' in request.extra)",
				},
				{
					Expression: "!('key2' in request.extra)",
				},
				{
					Expression: "('a' in request.extra['key1'])",
				},
				{
					Expression: "!('z' in request.extra['key1'])",
				},
				{
					Expression: "has(request.resourceAttributes) && request.resourceAttributes.namespace == 'kittensandponies'",
				},
			},
		},
		{
			name:               "match all except group, eval to one successful false, no error",
			attr:               aliceAttr,
			expectedCompileErr: "",
			expectedDecision:   authorizer.DecisionNoOpinion,
			expectedEvalErr:    "",
			expressions: []apiserver.WebhookMatchCondition{
				{
					Expression: "request.user == 'alice'",
				},
				{
					Expression: "request.uid == '1'",
				},
				{
					Expression: "('group3' in request.groups)",
				},
				{
					Expression: "has(request.resourceAttributes) && request.resourceAttributes.namespace == 'kittensandponies'",
				},
			},
		},
		{
			name:               "match condition with one compilation error",
			attr:               aliceAttr,
			expectedCompileErr: "matchConditions[2].expression: Invalid value: \"('group3' in request.group)\": compilation failed: ERROR: <input>:1:21: undefined field 'group'\n | ('group3' in request.group)\n | ....................^",
			expectedDecision:   authorizer.DecisionNoOpinion,
			expressions: []apiserver.WebhookMatchCondition{
				{
					Expression: "request.user == 'alice'",
				},
				{
					Expression: "request.uid == '1'",
				},
				{
					Expression: "('group3' in request.group)",
				},
				{
					Expression: "has(request.resourceAttributes) && request.resourceAttributes.namespace == 'kittensandponies'",
				},
			},
		},
		{
			name:               "match all except uid",
			attr:               aliceAttr,
			expectedCompileErr: "",
			expectedDecision:   authorizer.DecisionNoOpinion,
			expressions: []apiserver.WebhookMatchCondition{
				{
					Expression: "request.user == 'alice'",
				},
				{
					Expression: "request.uid == '2'",
				},
				{
					Expression: "('group1' in request.groups)",
				},
				{
					Expression: "has(request.resourceAttributes) && request.resourceAttributes.namespace == 'kittensandponies'",
				},
			},
		},
		{
			name:               "match on user name but not namespace",
			attr:               aliceAttr,
			expectedCompileErr: "",
			expectedDecision:   authorizer.DecisionNoOpinion,
			expressions: []apiserver.WebhookMatchCondition{
				{
					Expression: "request.user == 'alice'",
				},
				{
					Expression: "has(request.resourceAttributes) && request.resourceAttributes.namespace == 'kube-system'",
				},
			},
		},
		{
			name:               "mismatch on user name",
			attr:               bobAttr,
			expectedCompileErr: "",
			expectedDecision:   authorizer.DecisionNoOpinion,
			expressions: []apiserver.WebhookMatchCondition{
				{
					Expression: "request.user == 'alice'",
				},
			},
		},
		{
			name:               "match on user name but not resourceAttributes",
			attr:               bobAttr,
			expectedCompileErr: "",
			expectedDecision:   authorizer.DecisionNoOpinion,
			expressions: []apiserver.WebhookMatchCondition{
				{
					Expression: "request.user == 'bob'",
				},
				{
					Expression: "has(request.resourceAttributes) && request.resourceAttributes.namespace == 'kittensandponies'",
				},
			},
		},
		{
			name:               "expression failed to compile due to wrong return type",
			attr:               bobAttr,
			expectedCompileErr: `matchConditions[0].expression: Invalid value: "request.user": must evaluate to bool but got string`,
			expectedDecision:   authorizer.DecisionNoOpinion,
			expressions: []apiserver.WebhookMatchCondition{
				{
					Expression: "request.user",
				},
			},
		},
		{
			name:               "eval failed due to errors, no successful fail",
			attr:               alice2Attr,
			expectedCompileErr: "",
			expectedEvalErr:    "cel evaluation error: expression 'request.resourceAttributes.namespace == 'kittensandponies'' resulted in error: no such key: resourceAttributes",
			expectedDecision:   authorizer.DecisionNoOpinion,
			expressions: []apiserver.WebhookMatchCondition{
				{
					Expression: "request.user == 'alice2'",
				},
				{
					Expression: "request.resourceAttributes.namespace == 'kittensandponies'",
				},
			},
		},
		{
			name:               "at least one matchCondition successfully evaluates to FALSE, error ignored",
			attr:               alice2Attr,
			expectedCompileErr: "",
			expectedEvalErr:    "",
			expectedDecision:   authorizer.DecisionNoOpinion,
			expressions: []apiserver.WebhookMatchCondition{
				{
					Expression: "request.user != 'alice2'",
				},
				{
					Expression: "request.resourceAttributes.namespace == 'kittensandponies'",
				},
			},
		},
		{
			name:               "match on user name but failed to compile due to type check in nonResourceAttributes",
			attr:               bobAttr,
			expectedCompileErr: "matchConditions[1].expression: Invalid value: \"request.nonResourceAttributes.verb == 2\": compilation failed: ERROR: <input>:1:36: found no matching overload for '_==_' applied to '(string, int)'\n | request.nonResourceAttributes.verb == 2\n | ...................................^",
			expectedDecision:   authorizer.DecisionNoOpinion,
			expressions: []apiserver.WebhookMatchCondition{
				{
					Expression: "request.user == 'bob'",
				},
				{
					Expression: "request.nonResourceAttributes.verb == 2",
				},
			},
		},
		{
			name:               "match on user name and nonresourceAttributes",
			attr:               bobAttr,
			expectedCompileErr: "",
			expectedDecision:   authorizer.DecisionAllow,
			expressions: []apiserver.WebhookMatchCondition{
				{
					Expression: "request.user == 'bob'",
				},
				{
					Expression: "has(request.nonResourceAttributes) && request.nonResourceAttributes.verb == 'get'",
				},
			},
		},
		{
			name:               "match eval failed with bad SubjectAccessReviewSpec",
			attr:               authorizer.AttributesRecord{},
			expectedCompileErr: "",
			// default decisionOnError in newWithBackoff to skip
			expectedDecision: authorizer.DecisionNoOpinion,
			expectedEvalErr:  "cel evaluation error: expression 'request.resourceAttributes.verb == 'get'' resulted in error: no such key: resourceAttributes",
			expressions: []apiserver.WebhookMatchCondition{
				{
					Expression: "request.resourceAttributes.verb == 'get'",
				},
			},
		},
	}

	for i, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			wh, err := newV1Authorizer(s.URL, clientCert, clientKey, caCert, 0, noopAuthorizerMetrics(), authorizationcel.NewDefaultCompiler(), test.expressions, "")
			if len(test.expectedCompileErr) > 0 && err == nil {
				t.Fatalf("%d: Expected compile error", i)
			} else if len(test.expectedCompileErr) == 0 && err != nil {
				t.Fatalf("%d: unexpected error when creating a new WebhookAuthorizer: %v", i, err)
			}
			if err != nil {
				if d := cmp.Diff(test.expectedCompileErr, err.Error()); d != "" {
					t.Fatalf("newV1Authorizer mismatch (-want +got):\n%s", d)
				}
			}
			if err == nil {
				authorized, _, err := wh.Authorize(context.Background(), test.attr)
				if len(test.expectedEvalErr) > 0 && err == nil {
					t.Fatalf("%d: Expected eval error", i)
				} else if len(test.expectedEvalErr) == 0 && err != nil {
					t.Fatalf("%d: unexpected error when authorizing: %v", i, err)
				}

				if err != nil {
					if d := cmp.Diff(test.expectedEvalErr, err.Error()); d != "" {
						t.Fatalf("Authorize mismatch (-want +got):\n%s", d)
					}
				}

				if test.expectedDecision != authorized {
					t.Errorf("%d: expected authorized=%v, got %v", i, test.expectedDecision, authorized)
				}
			}
		})
	}
}

func noopAuthorizerMetrics() metrics.AuthorizerMetrics {
	return metrics.NoopAuthorizerMetrics{}
}

func celAuthorizerMetrics() metrics.AuthorizerMetrics {
	return celAuthorizerMetricsType{
		MatcherMetrics: authorizationcel.NewMatcherMetrics(),
	}
}

type celAuthorizerMetricsType struct {
	metrics.NoopRequestMetrics
	metrics.NoopWebhookMetrics
	authorizationcel.MatcherMetrics
}
