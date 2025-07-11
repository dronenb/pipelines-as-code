package github

import (
	"context"
	"crypto/hmac"
	"crypto/sha1"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"hash"
	"net/http"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/google/go-github/v71/github"
	"github.com/jonboulle/clockwork"
	"github.com/openshift-pipelines/pipelines-as-code/pkg/apis/pipelinesascode/keys"
	"github.com/openshift-pipelines/pipelines-as-code/pkg/apis/pipelinesascode/v1alpha1"
	"github.com/openshift-pipelines/pipelines-as-code/pkg/metrics"
	"github.com/openshift-pipelines/pipelines-as-code/pkg/params"
	"github.com/openshift-pipelines/pipelines-as-code/pkg/params/clients"
	"github.com/openshift-pipelines/pipelines-as-code/pkg/params/info"
	"github.com/openshift-pipelines/pipelines-as-code/pkg/params/settings"
	"github.com/openshift-pipelines/pipelines-as-code/pkg/params/triggertype"
	testclient "github.com/openshift-pipelines/pipelines-as-code/pkg/test/clients"
	ghtesthelper "github.com/openshift-pipelines/pipelines-as-code/pkg/test/github"
	"github.com/openshift-pipelines/pipelines-as-code/pkg/test/logger"
	"go.uber.org/zap"
	zapobserver "go.uber.org/zap/zaptest/observer"
	"gotest.tools/v3/assert"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"knative.dev/pkg/metrics/metricstest"
	_ "knative.dev/pkg/metrics/testing"
	"knative.dev/pkg/ptr"
	rtesting "knative.dev/pkg/reconciler/testing"
)

func TestGetTaskURI(t *testing.T) {
	tests := []struct {
		name       string
		wantErr    bool
		disallowed bool
		eventURL   string
		uri        string
		ret        string
	}{
		{
			name:     "Get Task URI",
			eventURL: "https://foo.com/owner/repo/pull/1",
			uri:      "https://foo.com/owner/repo/blob/main/file",
			wantErr:  false,
			ret:      "hello world",
		},
		{
			name:       "not comparable host",
			eventURL:   "https://foo/owner/repo/pull/1",
			uri:        "https://bar/owner/repo/blob/main/file",
			disallowed: true,
		},
		{
			name:       "bad uri",
			eventURL:   "https://foo/owner/repo/pull/1",
			uri:        "https://foo/owner/aooaooadoodao",
			disallowed: true,
			wantErr:    true,
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			sha := "sha"
			content := base64.StdEncoding.EncodeToString([]byte(tt.ret))
			ctx, _ := rtesting.SetupFakeContext(t)
			fakeclient, mux, _, teardown := ghtesthelper.SetupGH()
			defer teardown()
			provider := &Provider{ghClient: fakeclient}
			event := info.NewEvent()
			event.HeadBranch = "main"
			event.URL = tt.eventURL
			mux.HandleFunc("/repos/owner/repo/contents/file", func(rw http.ResponseWriter, _ *http.Request) {
				fmt.Fprintf(rw, `{"sha": "%s"}`, sha)
			})
			mux.HandleFunc(fmt.Sprintf("/repos/%s/%s/git/blobs/%s", "owner", "repo", sha), func(rw http.ResponseWriter, _ *http.Request) {
				fmt.Fprintf(rw, `{"content": "%s"}`, content)
			})
			allowed, content, err := provider.GetTaskURI(ctx, event, tt.uri)
			if (err != nil) != tt.wantErr {
				t.Errorf("GetTaskURI() error = %v, wantErr %v", err, tt.wantErr)
				return
			}
			if tt.disallowed && allowed {
				t.Errorf("GetTaskURI() is allowed and we want it to be disallowed")
				return
			} else if !tt.disallowed {
				return
			}
			if content != tt.ret {
				t.Errorf("GetTaskURI() got = %v, want %v", content, tt.ret)
			}
		})
	}
}

func TestGithubSplitURL(t *testing.T) {
	tests := []struct {
		name     string
		url      string
		wantOrg  string
		wantRepo string
		wantRef  string
		wantPath string
		gheURL   string
		wantErr  bool
	}{
		{
			name:     "Split URL",
			url:      "https://github.com/openshift-pipelines/pipelines-as-code/blob/main/testdatas/remote_task.yaml",
			wantOrg:  "openshift-pipelines",
			wantRepo: "pipelines-as-code",
			wantRef:  "main",
			wantPath: "testdatas/remote_task.yaml",
		},
		{
			name:     "Split URL with slash in branch",
			url:      "https://github.com/openshift-pipelines/pipelines-as-code/blob/feature%2Fbranch/testdatas/remote_task.yaml",
			wantOrg:  "openshift-pipelines",
			wantRepo: "pipelines-as-code",
			wantRef:  "feature/branch",
			wantPath: "testdatas/remote_task.yaml",
		},
		{
			name:     "Split URL with encoding emoji in branch",
			url:      "https://github.com/openshift-pipelines/pipelines-as-code/blob/%F0%9F%99%83/filename.yaml",
			wantOrg:  "openshift-pipelines",
			wantRepo: "pipelines-as-code",
			wantRef:  "🙃",
			wantPath: "filename.yaml",
		},
		{
			name:     "Split URL with url encoding emoji in filename",
			url:      "https://github.com/openshift-pipelines/pipelines-as-code/blob/branch/anemoji%F0%9F%99%83.yaml",
			wantOrg:  "openshift-pipelines",
			wantRepo: "pipelines-as-code",
			wantRef:  "branch",
			wantPath: "anemoji🙃.yaml",
		},
		{
			name:     "Split raw URL",
			url:      "https://raw.githubusercontent.com/openshift-pipelines/pipelines-as-code/main/testdatas/remote_task.yaml",
			wantOrg:  "openshift-pipelines",
			wantRepo: "pipelines-as-code",
			wantRef:  "main",
			wantPath: "testdatas/remote_task.yaml",
		},
		{
			name:     "Split raw URL2",
			url:      "https://raw.githubusercontent.com/openshift-pipelines/pipelines-as-code/main/remote_task.yaml",
			wantOrg:  "openshift-pipelines",
			wantRepo: "pipelines-as-code",
			wantRef:  "main",
			wantPath: "remote_task.yaml",
		},
		{
			name:    "Too small URL",
			url:     "https://raw.githubusercontent.com/openshift-pipelines/pipelines-as-code",
			wantErr: true,
		},
		{
			name:    "Invalid no path URL",
			url:     "https://raw.githubusercontent.com/openshift-pipelines/pipelines-as-code/main",
			wantErr: true,
		},
		{
			name:     "raw GHE URL",
			url:      "https://raw.ghe.domain.com/owner/repo/branch/file?token=TOKEN",
			gheURL:   "https://ghe.domain.com",
			wantOrg:  "owner",
			wantRepo: "repo",
			wantRef:  "branch",
			wantPath: "file",
		},
		{
			name:     "not matching ghe but allowed from public gh",
			url:      fmt.Sprintf("https://%s/owner/repo/branch/file?token=TOKEN", publicRawURLHost),
			gheURL:   "https://foo.com",
			wantOrg:  "owner",
			wantRepo: "repo",
			wantRef:  "branch",
			wantPath: "file",
		},
		{
			name:    "not matching raw",
			url:     "https://bar.com/owner/repo/branch/file?token=TOKEN",
			gheURL:  "https://foo.com",
			wantErr: true,
		},
		{
			name:    "not a full direct url",
			url:     "https://raw.ghe/owner/repo/branch?token=TOKEN",
			gheURL:  "https://raw.ghe",
			wantErr: true,
		},
		{
			name:    "bad formatted ghe url",
			url:     "https://raw.ghe/owner/repo/branch?token=TOKEN",
			gheURL:  "https:raw.ghe",
			wantErr: true,
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			event := info.NewEvent()
			event.GHEURL = tt.gheURL
			org, repo, path, ref, err := splitGithubURL(event, tt.url)
			if (err != nil) != tt.wantErr {
				t.Errorf("SplitURL() error = %v, wantErr %v", err, tt.wantErr)
				return
			}
			assert.Equal(t, tt.wantOrg, org)
			assert.Equal(t, tt.wantRepo, repo)
			assert.Equal(t, tt.wantRef, ref)
			assert.Equal(t, tt.wantPath, path)
		})
	}
}

func TestGetTektonDir(t *testing.T) {
	testGetTektonDir := []struct {
		treepath             string
		event                *info.Event
		name                 string
		expectedString       string
		provenance           string
		filterMessageSnippet string
		wantErr              string
		expectedGHApiCalls   int64
	}{
		{
			name: "test no subtree on pull request",
			event: &info.Event{
				Organization:  "tekton",
				Repository:    "cat",
				SHA:           "123",
				TriggerTarget: triggertype.PullRequest,
			},
			expectedString:       "PipelineRun",
			treepath:             "testdata/tree/simple",
			filterMessageSnippet: "Using PipelineRun definition from source pull_request tekton/cat#0",
			// 1. Get Repo root objects
			// 2. Get Tekton Dir objects
			// 3/4. Get object content for each object (pipelinerun.yaml, pipeline.yaml)
			expectedGHApiCalls: 4,
		},
		{
			name: "test no subtree on push",
			event: &info.Event{
				Organization:  "tekton",
				Repository:    "cat",
				SHA:           "123",
				TriggerTarget: triggertype.Push,
			},
			expectedString:       "PipelineRun",
			treepath:             "testdata/tree/simple",
			filterMessageSnippet: "Using PipelineRun definition from source push",
			// 1. Get Repo root objects
			// 2. Get Tekton Dir objects
			// 3/4. Get object content for each object (pipelinerun.yaml, pipeline.yaml)
			expectedGHApiCalls: 4,
		},
		{
			name: "test provenance default_branch ",
			event: &info.Event{
				Organization:  "tekton",
				Repository:    "cat",
				DefaultBranch: "main",
			},
			expectedString:       "FROMDEFAULTBRANCH",
			treepath:             "testdata/tree/defaultbranch",
			provenance:           "default_branch",
			filterMessageSnippet: "Using PipelineRun definition from default_branch: main",
			// 1. Get Repo root objects
			// 2. Get Tekton Dir objects
			// 3/4. Get object content for each object (pipelinerun.yaml, pipeline.yaml)
			expectedGHApiCalls: 4,
		},
		{
			name: "test with subtree",
			event: &info.Event{
				Organization: "tekton",
				Repository:   "cat",
				SHA:          "123",
			},
			expectedString: "FROMSUBTREE",
			treepath:       "testdata/tree/subdir",
			// 1. Get Repo root objects
			// 2. Get Tekton Dir objects
			// 3-5. Get object content for each object (foo/bar/pipeline.yaml)
			expectedGHApiCalls: 3,
		},
		{
			name: "test with badly formatted yaml",
			event: &info.Event{
				Organization: "tekton",
				Repository:   "cat",
				SHA:          "123",
			},
			expectedString: "FROMSUBTREE",
			treepath:       "testdata/tree/badyaml",
			wantErr:        "error unmarshalling yaml file badyaml.yaml: yaml: line 2: did not find expected key",
			// 1. Get Repo root objects
			// 2. Get Tekton Dir objects
			// 3. Get object content for object (badyaml.yaml)
			expectedGHApiCalls: 3,
		},
		{
			name: "test no tekton directory",
			event: &info.Event{
				Organization:  "tekton",
				Repository:    "cat",
				SHA:           "123",
				TriggerTarget: triggertype.PullRequest,
			},
			expectedString:       "",
			treepath:             "testdata/tree/notektondir",
			filterMessageSnippet: "Using PipelineRun definition from source pull_request tekton/cat#0",
			// 1. Get Repo root objects
			// _. No tekton dir to fetch
			expectedGHApiCalls: 1,
		},
		{
			name: "test tekton directory path is file",
			event: &info.Event{
				Organization: "tekton",
				Repository:   "cat",
				SHA:          "123",
			},
			treepath: "testdata/tree/tektondirisfile",
			wantErr:  ".tekton has been found but is not a directory",
			// 1. Get Repo root objects
			// _. Tekton dir is file, no directory to fetch
			expectedGHApiCalls: 1,
		},
	}
	for _, tt := range testGetTektonDir {
		t.Run(tt.name, func(t *testing.T) {
			resetMetrics()
			observer, exporter := zapobserver.New(zap.InfoLevel)
			fakelogger := zap.New(observer).Sugar()
			ctx, _ := rtesting.SetupFakeContext(t)
			fakeclient, mux, _, teardown := ghtesthelper.SetupGH()
			defer teardown()
			gvcs := Provider{
				ghClient:     fakeclient,
				providerName: "github",
				Logger:       fakelogger,
			}

			defer func() {
				if !t.Failed() {
					metricstest.CheckCountData(
						t,
						"pipelines_as_code_git_provider_api_request_count",
						map[string]string{"provider": "github"},
						tt.expectedGHApiCalls,
					)
				}
			}()

			if tt.provenance == "default_branch" {
				tt.event.SHA = tt.event.DefaultBranch
			} else {
				shaDir := fmt.Sprintf("%x", sha256.Sum256([]byte(tt.treepath)))
				tt.event.SHA = shaDir
			}
			ghtesthelper.SetupGitTree(t, mux, tt.treepath, tt.event, false)

			got, err := gvcs.GetTektonDir(ctx, tt.event, ".tekton", tt.provenance)
			if tt.wantErr != "" {
				assert.Assert(t, err != nil, "we should have get an error here")
				assert.ErrorContains(t, err, tt.wantErr)
				return
			}
			assert.NilError(t, err)

			var gotMatch bool
			if tt.expectedString == "" {
				gotMatch = got == tt.expectedString
			} else {
				gotMatch = strings.Contains(got, tt.expectedString)
			}

			assert.Assert(t, gotMatch, "expected %s, got %s", tt.expectedString, got)
			if tt.filterMessageSnippet != "" {
				gotcha := exporter.FilterMessageSnippet(tt.filterMessageSnippet)
				assert.Assert(t, gotcha.Len() > 0, "expected to find %s in logs, found %v", tt.filterMessageSnippet, exporter.All())
			}
		})
	}
}

func TestGetFileInsideRepo(t *testing.T) {
	testGetTektonDir := []struct {
		name       string
		rets       map[string]func(w http.ResponseWriter, r *http.Request)
		filepath   string
		wantErrStr string
	}{
		{
			name:     "fail/trying to get a subdir",
			filepath: "retdir",
			rets: map[string]func(w http.ResponseWriter, r *http.Request){
				"/repos/tekton/thecat/contents/retdir": func(w http.ResponseWriter, _ *http.Request) {
					fmt.Fprint(w, `[{"name": "directory", "path": "a/directory"}]`)
				},
			},

			wantErrStr: "referenced file inside the Github Repository retdir is a directory",
		},
		{
			name:     "fail/bad json",
			filepath: "retfile",
			rets: map[string]func(w http.ResponseWriter, r *http.Request){
				"/repos/tekton/thecat/contents/retfile": func(w http.ResponseWriter, _ *http.Request) {
					fmt.Fprint(w, `nonono`)
				},
			},

			wantErrStr: "invalid character",
		},
		{
			name:     "fail/bad encoding",
			filepath: "retfile",
			rets: map[string]func(w http.ResponseWriter, r *http.Request){
				"/repos/tekton/thecat/contents/retfile": func(w http.ResponseWriter, _ *http.Request) {
					fmt.Fprint(w, `{"name": "file", "path": "a/file", "sha": "shafile"}`)
				},
				"/repos/tekton/thecat/git/blobs/shafile": func(w http.ResponseWriter, _ *http.Request) {
					fmt.Fprint(w, `{"content": "xxxxxx", "sha": "shafile"}`)
				},
			},

			wantErrStr: "illegal base64 data",
		},
		{
			name:     "error/cannot get blob",
			filepath: "retfile",
			rets: map[string]func(w http.ResponseWriter, r *http.Request){
				"/repos/tekton/thecat/contents/retfile": func(w http.ResponseWriter, _ *http.Request) {
					fmt.Fprint(w, `{"name": "file", "path": "a/file", "sha": "shafile"}`)
				},
			},

			wantErrStr: "404",
		},
	}
	for _, tt := range testGetTektonDir {
		t.Run(tt.name, func(t *testing.T) {
			ctx, _ := rtesting.SetupFakeContext(t)
			fakeclient, mux, _, teardown := ghtesthelper.SetupGH()
			defer teardown()
			gvcs := Provider{
				ghClient: fakeclient,
			}
			for s, f := range tt.rets {
				mux.HandleFunc(s, f)
			}
			event := &info.Event{
				Organization: "tekton",
				Repository:   "thecat",
				SHA:          "123",
			}
			got, err := gvcs.GetFileInsideRepo(ctx, event, tt.filepath, "")
			if tt.wantErrStr != "" {
				assert.Assert(t, err != nil, "we should have get an error here")
				assert.Assert(t, strings.Contains(err.Error(), tt.wantErrStr), err.Error(), tt.wantErrStr)
				return
			}
			assert.NilError(t, err)
			assert.Assert(t, got != "")
		})
	}
}

func TestCheckSenderOrgMembership(t *testing.T) {
	tests := []struct {
		name      string
		apiReturn string
		allowed   bool
		wantErr   bool
		runevent  info.Event
	}{
		{
			name: "Check Sender Org Membership",
			runevent: info.Event{
				Organization: "organization",
				Sender:       "me",
			},
			apiReturn: `[{"login": "me"}]`,
			allowed:   true,
			wantErr:   false,
		},
		{
			name: "Check Sender not in Org Membership",
			runevent: info.Event{
				Organization: "organization",
				Sender:       "me",
			},
			apiReturn: `[{"login": "not"}]`,
			allowed:   false,
		},
		{
			name: "Not found on organization",
			runevent: info.Event{
				Organization: "notfound",
				Sender:       "me",
			},
			allowed: false,
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			fakeclient, mux, _, teardown := ghtesthelper.SetupGH()
			defer teardown()
			ctx, _ := rtesting.SetupFakeContext(t)
			gprovider := Provider{
				ghClient: fakeclient,
			}
			mux.HandleFunc(fmt.Sprintf("/orgs/%s/members", tt.runevent.Organization), func(rw http.ResponseWriter, _ *http.Request) {
				fmt.Fprint(rw, tt.apiReturn)
			})

			allowed, err := gprovider.checkSenderOrgMembership(ctx, &tt.runevent)
			if tt.wantErr && err == nil {
				t.Error("We didn't get an error when we wanted one")
			}
			if !tt.wantErr && err != nil {
				t.Errorf("We got an error when we didn't want it: %s", err)
			}
			assert.Equal(t, tt.allowed, allowed)
		})
	}
}

func TestGetStringPullRequestComment(t *testing.T) {
	regexp := `(^|\n)/retest(\r\n|$)`
	tests := []struct {
		name, apiReturn string
		wantErr         bool
		runevent        info.Event
		wantRet         bool
	}{
		{
			name:      "Get String from comments",
			runevent:  info.Event{URL: "http://1"},
			apiReturn: `[{"body": "/retest"}]`,
			wantRet:   true,
		},
		{
			name:      "Not matching string in comments",
			runevent:  info.Event{URL: "http://1"},
			apiReturn: `[{"body": ""}]`,
			wantRet:   false,
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			fakeclient, mux, _, teardown := ghtesthelper.SetupGH()
			defer teardown()
			ctx, _ := rtesting.SetupFakeContext(t)
			gprovider := Provider{
				ghClient: fakeclient,
			}
			mux.HandleFunc(fmt.Sprintf("/repos/issues/%s/comments", filepath.Base(tt.runevent.URL)), func(rw http.ResponseWriter, _ *http.Request) {
				fmt.Fprint(rw, tt.apiReturn)
			})

			ret, err := gprovider.GetStringPullRequestComment(ctx, &tt.runevent, regexp)
			if tt.wantErr && err == nil {
				t.Error("We didn't get an error when we wanted one")
			}
			if !tt.wantErr && err != nil {
				t.Errorf("We got an error when we didn't want it: %s", err)
			}

			if tt.wantRet {
				assert.Assert(t, ret != nil)
			}
		})
	}
}

func TestGithubGetCommitInfo(t *testing.T) {
	tests := []struct {
		name              string
		event             *info.Event
		noclient          bool
		apiReply, wantErr string
		shaurl, shatitle  string
	}{
		{
			name: "good",
			event: &info.Event{
				Organization: "owner",
				Repository:   "repository",
				SHA:          "shacommitinfo",
			},
			shaurl:   "https://git.provider/commit/info",
			shatitle: "My beautiful pony",
		},
		{
			name: "error",
			event: &info.Event{
				Organization: "owner",
				Repository:   "repository",
				SHA:          "shacommitinfo",
			},
			apiReply: "hello moto",
		},
		{
			name:     "noclient",
			event:    &info.Event{},
			wantErr:  "no github client has been initialized",
			noclient: true,
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			fakeclient, mux, _, teardown := ghtesthelper.SetupGH()
			defer teardown()
			mux.HandleFunc(fmt.Sprintf("/repos/%s/%s/git/commits/%s",
				tt.event.Organization, tt.event.Repository, tt.event.SHA), func(rw http.ResponseWriter, _ *http.Request) {
				if tt.apiReply != "" {
					fmt.Fprintf(rw, "%s", tt.apiReply)
					return
				}
				fmt.Fprintf(rw, `{"html_url": "%s", "message": "%s"}`, tt.shaurl, tt.shatitle)
			})
			ctx, _ := rtesting.SetupFakeContext(t)
			provider := &Provider{ghClient: fakeclient}
			if tt.noclient {
				provider = &Provider{}
			}
			err := provider.GetCommitInfo(ctx, tt.event)
			if tt.wantErr != "" {
				assert.ErrorContains(t, err, tt.wantErr)
				return
			}
			assert.Equal(t, tt.shatitle, tt.event.SHATitle)
			assert.Equal(t, tt.shaurl, tt.event.SHAURL)
		})
	}
}

func TestGithubSetClient(t *testing.T) {
	tests := []struct {
		name           string
		event          *info.Event
		expectedURL    string
		isGHE          bool
		installationID int64
	}{
		{
			name: "api url set",
			event: &info.Event{
				Provider: &info.Provider{
					URL: "foo.com",
				},
			},
			expectedURL:    "https://foo.com",
			isGHE:          true,
			installationID: 0,
		},
		{
			name:           "default to public github",
			expectedURL:    fmt.Sprintf("%s/", keys.PublicGithubAPIURL),
			event:          info.NewEvent(),
			installationID: 12345,
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			tt.event.InstallationID = tt.installationID
			ctx, _ := rtesting.SetupFakeContext(t)
			core, observer := zapobserver.New(zap.InfoLevel)
			testLog := zap.New(core).Sugar()
			fakeRun := &params.Run{
				Clients: clients.Clients{
					Log: testLog,
				},
			}
			v := Provider{}
			err := v.SetClient(ctx, fakeRun, tt.event, nil, nil)
			assert.NilError(t, err)
			assert.Equal(t, tt.expectedURL, *v.APIURL)
			assert.Equal(t, "https", v.Client().BaseURL.Scheme)
			if tt.isGHE {
				assert.Equal(t, "/api/v3/", v.Client().BaseURL.Path)
			} else {
				assert.Equal(t, "/", v.Client().BaseURL.Path)
			}

			logs := observer.TakeAll()
			assert.Assert(t, len(logs) == 1, "expected exactly one log entry, got %d", len(logs))

			prefix := "github-webhook"
			if tt.installationID != 0 {
				prefix = "github-app"
			}
			wantStart := fmt.Sprintf("%s: initialized OAuth2 client", prefix)
			got := logs[0].Message
			assert.Assert(t, strings.HasPrefix(got, wantStart), "log entry should start with %q, got %q", wantStart, got)

			// Determine expected providerName based on whether it's GHE or public GitHub.
			expectedProviderName := "github"
			if tt.isGHE {
				expectedProviderName = "github-enterprise"
			}

			// Build the full expected log message.
			fullExpected := fmt.Sprintf(
				"%s: initialized OAuth2 client for providerName=%s providerURL=%s",
				prefix,
				expectedProviderName,
				tt.event.Provider.URL,
			)

			assert.Equal(t, fullExpected, logs[0].Message)
		})
	}
}

func TestValidate(t *testing.T) {
	header := http.Header{}
	header.Set(github.SHA256SignatureHeader, "hello")
	tests := []struct {
		name         string
		wantErr      bool
		secret       string
		payload      string
		hashFunc     func() hash.Hash
		header       string
		prefixheader string
	}{
		{
			name:         "good/SHA256Signature",
			secret:       "secrete",
			payload:      `{"hello": "moto"}`,
			hashFunc:     sha256.New,
			header:       github.SHA256SignatureHeader,
			prefixheader: "sha256",
		},
		{
			name:         "good/SHA1Signature",
			secret:       "secrete",
			payload:      `{"ola": "amigo"}`,
			hashFunc:     sha1.New,
			header:       github.SHA1SignatureHeader,
			prefixheader: "sha1",
		},
		{
			name:         "bad/signature",
			payload:      `{"ciao": "ragazzo"}`,
			hashFunc:     sha256.New,
			header:       github.SHA1SignatureHeader,
			prefixheader: "sha1",
			wantErr:      true,
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			logger, _ := logger.GetLogger()
			v := &Provider{Logger: logger}

			hm := hmac.New(tt.hashFunc, []byte(tt.secret))
			hm.Write([]byte(tt.payload))
			signature := hex.EncodeToString(hm.Sum(nil))

			httpHeader := http.Header{}
			httpHeader.Add(tt.header, fmt.Sprintf("%s=%s", tt.prefixheader, signature))

			event := info.NewEvent()
			event.Request = &info.Request{
				Header:  httpHeader,
				Payload: []byte(tt.payload),
			}
			event.Provider = &info.Provider{
				WebhookSecret: tt.secret,
			}

			if err := v.Validate(context.TODO(), nil, event); (err != nil) != tt.wantErr {
				t.Errorf("Validate() error = %v, wantErr %v", err, tt.wantErr)
			}
		})
	}
}

func TestGetFiles(t *testing.T) {
	tests := []struct {
		name                   string
		event                  *info.Event
		commitFiles            []*github.CommitFile
		commit                 *github.RepositoryCommit
		wantAddedFilesCount    int
		wantDeletedFilesCount  int
		wantModifiedFilesCount int
		wantRenamedFilesCount  int
	}{
		{
			name: "pull-request",
			event: &info.Event{
				TriggerTarget:     "pull_request",
				Organization:      "pullrequestowner",
				Repository:        "pullrequestrepository",
				PullRequestNumber: 10,
			},
			commitFiles: []*github.CommitFile{
				{
					Filename: ptr.String("modified.yaml"),
					Status:   ptr.String("modified"),
				}, {
					Filename: ptr.String("added.doc"),
					Status:   ptr.String("added"),
				}, {
					Filename: ptr.String("removed.yaml"),
					Status:   ptr.String("removed"),
				}, {
					Filename: ptr.String("renamed.doc"),
					Status:   ptr.String("renamed"),
				},
			},
			wantAddedFilesCount:    1,
			wantDeletedFilesCount:  1,
			wantModifiedFilesCount: 1,
			wantRenamedFilesCount:  1,
		},
		{
			name: "push",
			event: &info.Event{
				TriggerTarget: "push",
				Organization:  "pushrequestowner",
				Repository:    "pushrequestrepository",
				SHA:           "shacommitinfo",
			},
			commit: &github.RepositoryCommit{
				Files: []*github.CommitFile{
					{
						Filename: ptr.String("modified.yaml"),
						Status:   ptr.String("modified"),
					}, {
						Filename: ptr.String("added.doc"),
						Status:   ptr.String("added"),
					}, {
						Filename: ptr.String("removed.yaml"),
						Status:   ptr.String("removed"),
					}, {
						Filename: ptr.String("renamed.doc"),
						Status:   ptr.String("renamed"),
					},
				},
			},
			wantAddedFilesCount:    1,
			wantDeletedFilesCount:  1,
			wantModifiedFilesCount: 1,
			wantRenamedFilesCount:  1,
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			fakeclient, mux, _, teardown := ghtesthelper.SetupGH()
			defer teardown()
			prCommitFiles := []*github.CommitFile{
				{
					Filename: ptr.String("modified.yaml"),
					Status:   ptr.String("modified"),
				}, {
					Filename: ptr.String("added.doc"),
					Status:   ptr.String("added"),
				}, {
					Filename: ptr.String("removed.yaml"),
					Status:   ptr.String("removed"),
				}, {
					Filename: ptr.String("renamed.doc"),
					Status:   ptr.String("renamed"),
				},
			}

			pushCommitFiles := []*github.CommitFile{
				{
					Filename: ptr.String("modified.yaml"),
					Status:   ptr.String("modified"),
				}, {
					Filename: ptr.String("added.doc"),
					Status:   ptr.String("added"),
				}, {
					Filename: ptr.String("removed.yaml"),
					Status:   ptr.String("removed"),
				}, {
					Filename: ptr.String("renamed.doc"),
					Status:   ptr.String("renamed"),
				},
			}
			if tt.event.TriggerTarget == "pull_request" {
				mux.HandleFunc(fmt.Sprintf("/repos/%s/%s/pulls/%d/files",
					tt.event.Organization, tt.event.Repository, tt.event.PullRequestNumber), func(rw http.ResponseWriter, r *http.Request) {
					if r.URL.Query().Get("page") == "" || r.URL.Query().Get("page") == "1" {
						rw.Header().Add("Link", fmt.Sprintf("<https://api.github.com/repos/%s/%s/pulls/%d/files?page=2>; rel=\"next\"", tt.event.Organization, tt.event.Repository, tt.event.PullRequestNumber))
						fmt.Fprint(rw, "[]")
					} else {
						b, _ := json.Marshal(prCommitFiles)
						fmt.Fprint(rw, string(b))
					}
				})
			}
			if tt.event.TriggerTarget == "push" {
				mux.HandleFunc(fmt.Sprintf("/repos/%s/%s/commits/%s",
					tt.event.Organization, tt.event.Repository, tt.event.SHA), func(rw http.ResponseWriter, _ *http.Request) {
					c := &github.RepositoryCommit{
						Files: pushCommitFiles,
					}
					b, _ := json.Marshal(c)
					fmt.Fprint(rw, string(b))
				})
			}

			ctx, _ := rtesting.SetupFakeContext(t)
			provider := &Provider{
				ghClient:      fakeclient,
				PaginedNumber: 1,
			}
			changedFiles, err := provider.GetFiles(ctx, tt.event)
			assert.NilError(t, err, nil)
			assert.Equal(t, tt.wantAddedFilesCount, len(changedFiles.Added))
			assert.Equal(t, tt.wantDeletedFilesCount, len(changedFiles.Deleted))
			assert.Equal(t, tt.wantModifiedFilesCount, len(changedFiles.Modified))
			assert.Equal(t, tt.wantRenamedFilesCount, len(changedFiles.Renamed))

			if tt.event.TriggerTarget == "pull_request" {
				for i := range changedFiles.All {
					assert.Equal(t, *tt.commitFiles[i].Filename, changedFiles.All[i])
				}
			}
			if tt.event.TriggerTarget == "push" {
				for i := range changedFiles.All {
					assert.Equal(t, *tt.commit.Files[i].Filename, changedFiles.All[i])
				}
			}
		})
	}
}

type roundTripperFunc func(*http.Request) (*http.Response, error)

func (f roundTripperFunc) RoundTrip(r *http.Request) (*http.Response, error) {
	return f(r)
}

func TestProvider_checkWebhookSecretValidity(t *testing.T) {
	t1 := time.Date(1999, time.February, 3, 4, 5, 6, 7, time.UTC)
	cw := clockwork.NewFakeClockAt(t1)
	tests := []struct {
		name           string
		wantSubErr     string
		remaining      int
		expTime        time.Time
		expHeaderSet   bool
		apiNotEnabled  bool
		wantLogSnippet string
		statusCode     int
		wantNilSCIM    bool
		wantNilResp    bool
	}{
		{
			name:         "remaining scim calls",
			remaining:    1,
			expHeaderSet: true,
			expTime:      cw.Now().Add(1 * time.Minute),
		},
		{
			name:         "no remaining scim calls",
			wantSubErr:   "api rate limit exceeded. Access will be restored at Mon, 01 Jan 0001 00:00:00 UTC",
			remaining:    0,
			expHeaderSet: true,
			expTime:      cw.Now().Add(1 * time.Minute),
		},
		{
			name:         "expired",
			wantSubErr:   "token has expired",
			expTime:      cw.Now().Add(-1 * time.Minute),
			expHeaderSet: true,
		},
		{
			name:      "no header mean unlimited",
			remaining: 5,
		},
		{
			name:       "skipping api rate limit is not enabled",
			remaining:  0,
			statusCode: http.StatusNotFound,
		},
		{
			name:        "skipping because scim is not available",
			remaining:   0,
			wantNilSCIM: true,
		},
		{
			name:        "resp is nil",
			remaining:   0,
			wantNilResp: true,
			wantSubErr:  "error making request to the GitHub API checking rate limit",
		},
		{
			name:       "no header but no remaining scim calls",
			remaining:  0,
			wantSubErr: "api rate limit exceeded. Access will be restored at Mon, 01 Jan 0001 00:00:00 UTC",
		},
		{
			name:       "api error",
			wantSubErr: "error making request to the GitHub API checking rate limit",
			statusCode: http.StatusInternalServerError,
		},
		{
			name:           "not enabled",
			apiNotEnabled:  true,
			wantLogSnippet: "skipping checking",
		},
		{
			name:           "not enabled",
			apiNotEnabled:  true,
			wantLogSnippet: "skipping checking",
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			ctx, _ := rtesting.SetupFakeContext(t)
			fakeclient, mux, _, teardown := ghtesthelper.SetupGH()
			logger, observer := logger.GetLogger()

			if !tt.apiNotEnabled {
				mux.HandleFunc("/rate_limit", func(rw http.ResponseWriter, _ *http.Request) {
					if tt.statusCode != 0 {
						rw.WriteHeader(tt.statusCode)
						return
					}
					s := &github.RateLimits{}
					if !tt.wantNilSCIM {
						s.SCIM = &github.Rate{
							Remaining: tt.remaining,
						}
					}
					st := new(struct {
						Resources *github.RateLimits `json:"resources"`
					})
					st.Resources = s
					b, _ := json.Marshal(st)
					rw.Header().Set("Content-Type", "application/json")
					if tt.expHeaderSet {
						rw.Header().Set("GitHub-Authentication-Token-Expiration", tt.expTime.Format("2006-01-02 03:04:05 MST"))
					}
					fmt.Fprint(rw, string(b))
				})
			}
			defer teardown()

			// create bad round tripper to make response nil and test that it handles that case.
			if tt.wantNilResp {
				errRT := roundTripperFunc(func(*http.Request) (*http.Response, error) {
					return nil, fmt.Errorf("network down")
				})
				httpClient := &http.Client{Transport: errRT}
				fakeclient = github.NewClient(httpClient)
			}

			v := &Provider{
				ghClient: fakeclient,
				Logger:   logger,
			}
			err := v.checkWebhookSecretValidity(ctx, cw)
			if tt.wantSubErr != "" {
				assert.ErrorContains(t, err, tt.wantSubErr)
			} else {
				assert.NilError(t, err)
			}

			if tt.wantLogSnippet != "" {
				assert.Assert(t, observer.FilterMessageSnippet(tt.wantLogSnippet).Len() > 0)
			}
		})
	}
}

func TestParseTS(t *testing.T) {
	tests := []struct {
		name    string
		wantErr bool
		expTS   string
	}{
		{
			name:  "valid as defined by go-github",
			expTS: "2023-01-31 03:30:00 UTC",
		},
		{
			name:  "valid with UTC and eu time",
			expTS: "2023-01-31 15:30:00 UTC",
		},
		{
			name:  "valid with timezone inside",
			expTS: "2023-04-26 23:23:26 +2000",
		},

		{
			name:    "invalid",
			expTS:   "Mon 2023-04-26 23:23:26 +2000",
			wantErr: true,
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, err := parseTS(tt.expTS)
			if tt.wantErr {
				assert.Assert(t, err != nil)
			} else {
				assert.NilError(t, err)
				assert.Assert(t, got.Year() != 0o1)
			}
		})
	}
}

func TestListRepos(t *testing.T) {
	fakeclient, mux, _, teardown := ghtesthelper.SetupGH()
	defer teardown()

	mux.HandleFunc("user/installations/1/repositories/2", func(rw http.ResponseWriter, _ *http.Request) {
		fmt.Fprint(rw)
	})

	mux.HandleFunc("/installation/repositories", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Authorization", "Bearer 12345")
		w.Header().Set("Accept", "application/vnd.github+json")
		if r.URL.Query().Get("page") == "" || r.URL.Query().Get("page") == "1" {
			w.Header().Add("Link", `<https://api.github.com/installation/repositories?page=2&per_page=1>; rel="next"`)
			_, _ = fmt.Fprint(w, `{}`)
		} else {
			_, _ = fmt.Fprint(w, `{"total_count": 1,"repositories": [{"id":1,"html_url": "https://matched/by/incoming"}]}`)
		}
	})

	ctx, _ := rtesting.SetupFakeContext(t)
	provider := &Provider{ghClient: fakeclient, PaginedNumber: 1}
	data, err := ListRepos(ctx, provider)
	assert.NilError(t, err)
	assert.Equal(t, data[0], "https://matched/by/incoming")
}

func TestCreateToken(t *testing.T) {
	repos := []v1alpha1.Repository{{
		ObjectMeta: metav1.ObjectMeta{
			Name:      "repo1",
			Namespace: "test1",
		},
		Spec: v1alpha1.RepositorySpec{
			ConcurrencyLimit: nil,
			URL:              "https://github.com/owner/project1",
			Settings: &v1alpha1.Settings{
				GithubAppTokenScopeRepos: []string{"owner/project1", "owner/project2"},
			},
		},
	}, {
		ObjectMeta: metav1.ObjectMeta{
			Name:      "repo2",
			Namespace: "test1",
		},
		Spec: v1alpha1.RepositorySpec{
			ConcurrencyLimit: nil,
			URL:              "https://github.com/owner/project2",
		},
	}}

	ctx, _ := rtesting.SetupFakeContext(t)
	logger, _ := logger.GetLogger()
	testNamespace := &corev1.Namespace{
		ObjectMeta: metav1.ObjectMeta{
			Name: "pipelinesascode",
		},
	}
	validSecret := &corev1.Secret{
		ObjectMeta: metav1.ObjectMeta{
			Name:      "pipelines-as-code-secret",
			Namespace: testNamespace.Name,
		},
		Data: map[string][]byte{
			"github-application-id": []byte("12345"),
			"github-private-key":    []byte(fakePrivateKey),
		},
	}
	ctx = info.StoreNS(ctx, testNamespace.GetName())
	ctx = info.StoreCurrentControllerName(ctx, "default")

	tdata := testclient.Data{
		Namespaces: []*corev1.Namespace{testNamespace},
		Secret:     []*corev1.Secret{validSecret},
	}

	stdata, _ := testclient.SeedTestData(t, ctx, tdata)

	run := &params.Run{
		Clients: clients.Clients{
			Log:            logger,
			PipelineAsCode: stdata.PipelineAsCode,
			Kube:           stdata.Kube,
		},
		Info: info.Info{
			Controller: &info.ControllerInfo{
				Secret: validSecret.GetName(),
			},
		},
	}

	info := &info.Event{
		Provider: &info.Provider{
			URL: "",
		},
		InstallationID: int64(1234567),
	}

	fakeclient, mux, _, teardown := ghtesthelper.SetupGH()
	defer teardown()

	extraRepoInstallIDs := map[string]string{"owner/project1": "789", "owner/project2": "10112"}
	urlData := []string{}
	for _, r := range repos {
		if r.Spec.Settings != nil {
			urlData = append(urlData, r.Spec.Settings.GithubAppTokenScopeRepos...)
		}
	}
	for _, v := range urlData {
		split := strings.Split(v, "/")
		mux.HandleFunc(fmt.Sprintf("/repos/%s/%s", split[0], split[1]), func(w http.ResponseWriter, _ *http.Request) {
			sid := extraRepoInstallIDs[fmt.Sprintf("%s/%s", split[0], split[1])]
			_, _ = fmt.Fprintf(w, `{"id": %s}`, sid)
		})
	}

	provider := &Provider{ghClient: fakeclient}
	provider.Run = run
	_, err := provider.CreateToken(ctx, urlData, info)
	assert.Assert(t, len(provider.RepositoryIDs) == 2, "found repositoryIDs are %d which is less than expected", len(provider.RepositoryIDs))
	if err != nil {
		assert.ErrorContains(t, err, "could not refresh installation id 1234567's token")
	}
}

func TestIsHeadCommitOfBranch(t *testing.T) {
	tests := []struct {
		name       string
		sha        string
		branchName string
		wantErr    bool
	}{{
		name:    "sha exist in the branch",
		sha:     "SHA1",
		wantErr: false,
	}, {
		name:    "sha doesn't exist in the branch",
		sha:     "SHA2",
		wantErr: true,
	}}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			runEvent := &info.Event{
				Organization:   "pushrequestowner",
				Repository:     "pushrequestrepository",
				SHA:            tt.sha,
				InstallationID: int64(1234567),
			}
			fakeclient, mux, _, teardown := ghtesthelper.SetupGH()
			defer teardown()

			mux.HandleFunc(fmt.Sprintf("/repos/%s/%s/branches/test1",
				runEvent.Organization, runEvent.Repository), func(rw http.ResponseWriter, _ *http.Request) {
				_, err := fmt.Fprintf(rw, `{
			"name": "test1",
			"commit": {
				"sha": "SHA1"
			}}}`)
				assert.NilError(t, err)
			})

			ctx, _ := rtesting.SetupFakeContext(t)
			provider := &Provider{ghClient: fakeclient}
			err := provider.isHeadCommitOfBranch(ctx, runEvent, "test1")
			assert.Equal(t, err != nil, tt.wantErr)
		})
	}
}

func resetMetrics() {
	metricstest.Unregister(
		"pipelines_as_code_pipelinerun_count",
		"pipelines_as_code_pipelinerun_duration_seconds_sum",
		"pipelines_as_code_running_pipelineruns_count",
		"pipelines_as_code_git_provider_api_request_count",
	)

	// have to reset sync.Once to allow recreation of Recorder.
	metrics.ResetRecorder()
}

func TestCreateComment(t *testing.T) {
	tests := []struct {
		name          string
		event         *info.Event
		updateMarker  string
		mockResponses map[string]func(rw http.ResponseWriter, _ *http.Request)
		wantErr       string
		clientNil     bool
	}{
		{
			name:      "nil client error",
			clientNil: true,
			event:     &info.Event{PullRequestNumber: 123},
			wantErr:   "no github client has been initialized",
		},
		{
			name:    "not a pull request error",
			event:   &info.Event{PullRequestNumber: 0},
			wantErr: "create comment only works on pull requests",
		},
		{
			name:         "create new comment",
			event:        &info.Event{Organization: "org", Repository: "repo", PullRequestNumber: 123},
			updateMarker: "",
			mockResponses: map[string]func(rw http.ResponseWriter, _ *http.Request){
				"/repos/org/repo/issues/123/comments": func(rw http.ResponseWriter, r *http.Request) {
					assert.Equal(t, r.Method, http.MethodPost)
					rw.WriteHeader(http.StatusCreated)
				},
			},
		},
		{
			name:         "update existing comment",
			event:        &info.Event{Organization: "org", Repository: "repo", PullRequestNumber: 123},
			updateMarker: "MARKER",
			mockResponses: map[string]func(rw http.ResponseWriter, _ *http.Request){
				"/repos/org/repo/issues/123/comments": func(rw http.ResponseWriter, r *http.Request) {
					if r.Method == http.MethodGet {
						fmt.Fprint(rw, `[{"id": 555, "body": "MARKER"}]`)
						return
					}
				},
				"/repos/org/repo/issues/comments/555": func(rw http.ResponseWriter, r *http.Request) {
					assert.Equal(t, r.Method, http.MethodPatch)
					rw.WriteHeader(http.StatusOK)
				},
			},
		},
		{
			name:         "no matching comment creates new",
			event:        &info.Event{Organization: "org", Repository: "repo", PullRequestNumber: 123},
			updateMarker: "MARKER",
			mockResponses: map[string]func(rw http.ResponseWriter, _ *http.Request){
				"/repos/org/repo/issues/123/comments": func(rw http.ResponseWriter, r *http.Request) {
					if r.Method == http.MethodGet {
						fmt.Fprint(rw, `[{"id": 555, "body": "NO_MATCH"}]`)
						return
					}
					assert.Equal(t, r.Method, http.MethodPost)
					rw.WriteHeader(http.StatusCreated)
				},
			},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			ctx, _ := rtesting.SetupFakeContext(t)

			var provider *Provider
			if !tt.clientNil {
				fakeclient, mux, _, teardown := ghtesthelper.SetupGH()
				defer teardown()
				provider = &Provider{ghClient: fakeclient}

				for pattern, handler := range tt.mockResponses {
					mux.HandleFunc(pattern, handler)
				}
			} else {
				provider = &Provider{} // nil client
			}

			err := provider.CreateComment(ctx, tt.event, "comment body", tt.updateMarker)

			if tt.wantErr != "" {
				assert.ErrorContains(t, err, tt.wantErr)
				return
			}
			assert.NilError(t, err)
		})
	}
}

func TestSkipPushEventForPRCommits(t *testing.T) {
	iid := int64(1234)
	tests := []struct {
		name                string
		pacInfoEnabled      bool
		pushEvent           *github.PushEvent
		mockAPIs            map[string]func(rw http.ResponseWriter, r *http.Request)
		isPartOfPR          bool
		wantErr             bool
		wantErrContains     string
		skipWarnLogContains string
	}{
		{
			name:           "skip push event when commit is part of an open PR",
			pacInfoEnabled: true,
			pushEvent: &github.PushEvent{
				Repo: &github.PushEventRepository{
					Name:  github.Ptr("testRepo"),
					Owner: &github.User{Login: github.Ptr("testOrg")},
				},
				HeadCommit: &github.HeadCommit{
					ID: github.Ptr("abc123"),
				},
			},
			mockAPIs: map[string]func(rw http.ResponseWriter, r *http.Request){
				"/repos/testOrg/testRepo/commits/abc123/pulls": func(rw http.ResponseWriter, r *http.Request) {
					assert.Equal(t, r.Method, http.MethodGet)
					fmt.Fprint(rw, `[{"number": 42, "state": "open"}]`)
				},
			},
			isPartOfPR:      true,
			wantErr:         true,
			wantErrContains: "commit abc123 is part of pull request #42, skipping push event",
		},
		{
			name:           "continue processing push event when commit is not part of PR",
			pacInfoEnabled: true,
			pushEvent: &github.PushEvent{
				Repo: &github.PushEventRepository{
					Name:          github.Ptr("testRepo"),
					Owner:         &github.User{Login: github.Ptr("testOrg")},
					DefaultBranch: github.Ptr("main"),
					HTMLURL:       github.Ptr("https://github.com/testOrg/testRepo"),
					ID:            github.Ptr(iid),
				},
				HeadCommit: &github.HeadCommit{
					ID:      github.Ptr("abc123"),
					URL:     github.Ptr("https://github.com/testOrg/testRepo/commit/abc123"),
					Message: github.Ptr("Test commit message"),
				},
				Ref:    github.Ptr("refs/heads/main"),
				Sender: &github.User{Login: github.Ptr("testUser")},
			},
			mockAPIs: map[string]func(rw http.ResponseWriter, r *http.Request){
				"/repos/testOrg/testRepo/pulls": func(rw http.ResponseWriter, r *http.Request) {
					assert.Equal(t, r.Method, http.MethodGet)
					assert.Equal(t, r.URL.Query().Get("state"), "open")
					fmt.Fprint(rw, `[{"number": 42}]`)
				},
				"/repos/testOrg/testRepo/pulls/42/commits": func(rw http.ResponseWriter, r *http.Request) {
					assert.Equal(t, r.Method, http.MethodGet)
					fmt.Fprint(rw, `[{"sha": "def456"}, {"sha": "xyz789"}]`)
				},
			},
			isPartOfPR: false,
			wantErr:    false,
		},
		{
			name:           "continue when skip feature is disabled",
			pacInfoEnabled: false,
			pushEvent: &github.PushEvent{
				Repo: &github.PushEventRepository{
					Name:          github.Ptr("testRepo"),
					Owner:         &github.User{Login: github.Ptr("testOrg")},
					DefaultBranch: github.Ptr("main"),
					HTMLURL:       github.Ptr("https://github.com/testOrg/testRepo"),
					ID:            github.Ptr(iid),
				},
				HeadCommit: &github.HeadCommit{
					ID:      github.Ptr("abc123"),
					URL:     github.Ptr("https://github.com/testOrg/testRepo/commit/abc123"),
					Message: github.Ptr("Test commit message"),
				},
				Ref:    github.Ptr("refs/heads/main"),
				Sender: &github.User{Login: github.Ptr("testUser")},
			},
			isPartOfPR: false, // This should not be checked when feature is disabled
			wantErr:    false,
		},
		{
			name:           "log warning when API error occurs",
			pacInfoEnabled: true,
			pushEvent: &github.PushEvent{
				Repo: &github.PushEventRepository{
					Name:  github.Ptr("testRepo"),
					Owner: &github.User{Login: github.Ptr("testOrg")},
				},
				HeadCommit: &github.HeadCommit{
					ID: github.Ptr("1234"),
				},
			},
			mockAPIs: map[string]func(rw http.ResponseWriter, r *http.Request){
				"/repos/testOrg/testRepo/pulls": func(rw http.ResponseWriter, _ *http.Request) {
					rw.WriteHeader(http.StatusInternalServerError)
					fmt.Fprint(rw, `{"message": "API error"}`)
				},
			},
			isPartOfPR:          false,
			wantErr:             false,
			skipWarnLogContains: "Error getting pull requests associated with the commit in this push event",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			ctx, _ := rtesting.SetupFakeContext(t)
			fakeclient, mux, _, teardown := ghtesthelper.SetupGH()
			defer teardown()

			// Register API endpoints
			for pattern, handler := range tt.mockAPIs {
				mux.HandleFunc(pattern, handler)
			}

			// Create a logger that captures logs
			observer, logs := zapobserver.New(zap.InfoLevel)
			logger := zap.New(observer).Sugar()

			// Create provider with the test configuration
			provider := &Provider{
				ghClient: fakeclient,
				Logger:   logger,
				pacInfo: &info.PacOpts{
					Settings: settings.Settings{
						SkipPushEventForPRCommits: tt.pacInfoEnabled,
					},
				},
			}

			// Create event with the right trigger type
			event := info.NewEvent()
			event.TriggerTarget = triggertype.Push

			// Process the event
			result, err := provider.processEvent(ctx, event, tt.pushEvent)

			// Check errors if expected
			if tt.wantErr {
				assert.Assert(t, err != nil)
				if tt.wantErrContains != "" {
					assert.ErrorContains(t, err, tt.wantErrContains)
				}
				assert.Assert(t, result == nil, "Expected nil result when error occurs")
				return
			}

			// If no error expected, check the result
			assert.NilError(t, err)
			assert.Assert(t, result != nil, "Expected non-nil result when no error occurs")

			// Check event fields were properly processed
			if !tt.pacInfoEnabled || !tt.isPartOfPR {
				assert.Equal(t, result.Organization, tt.pushEvent.GetRepo().GetOwner().GetLogin())
				assert.Equal(t, result.Repository, tt.pushEvent.GetRepo().GetName())
				assert.Equal(t, result.SHA, tt.pushEvent.GetHeadCommit().GetID())
				assert.Equal(t, result.Sender, tt.pushEvent.GetSender().GetLogin())
			}

			// Check for warning logs if applicable
			if tt.skipWarnLogContains != "" {
				// Look for warning logs
				found := false
				for _, logEntry := range logs.All() {
					if strings.Contains(logEntry.Message, tt.skipWarnLogContains) {
						found = true
						break
					}
				}
				assert.Assert(t, found, "Expected warning log containing: %s", tt.skipWarnLogContains)
			}
		})
	}
}
