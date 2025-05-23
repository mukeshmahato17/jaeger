// Copyright (c) 2019 The Jaeger Authors.
// Copyright (c) 2017 Uber Technologies, Inc.
// SPDX-License-Identifier: Apache-2.0

package elasticsearch

import (
	"context"
	"encoding/base64"
	"errors"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/mock"
	"github.com/stretchr/testify/require"
	"go.uber.org/zap"
	"go.uber.org/zap/zaptest"

	"github.com/jaegertracing/jaeger-idl/model/v1"
	"github.com/jaegertracing/jaeger/internal/config"
	"github.com/jaegertracing/jaeger/internal/metrics"
	es "github.com/jaegertracing/jaeger/internal/storage/elasticsearch"
	escfg "github.com/jaegertracing/jaeger/internal/storage/elasticsearch/config"
	"github.com/jaegertracing/jaeger/internal/storage/elasticsearch/mocks"
	"github.com/jaegertracing/jaeger/internal/storage/v1/api/spanstore"
	"github.com/jaegertracing/jaeger/internal/testutils"
)

var mockEsServerResponse = []byte(`
{
	"Version": {
		"Number": "6"
	}
}
`)

type mockClientBuilder struct {
	err                 error
	createTemplateError error
}

func (m *mockClientBuilder) NewClient(*escfg.Configuration, *zap.Logger, metrics.Factory) (es.Client, error) {
	if m.err == nil {
		c := &mocks.Client{}
		tService := &mocks.TemplateCreateService{}
		tService.On("Body", mock.Anything).Return(tService)
		tService.On("Do", context.Background()).Return(nil, m.createTemplateError)
		c.On("CreateTemplate", mock.Anything).Return(tService)
		c.On("GetVersion").Return(uint(6))
		c.On("Close").Return(nil)
		return c, nil
	}
	return nil, m.err
}

func TestElasticsearchFactory(t *testing.T) {
	f := NewFactory()
	v, command := config.Viperize(f.AddFlags)
	command.ParseFlags([]string{})
	f.InitFromViper(v, zap.NewNop())

	f.newClientFn = (&mockClientBuilder{err: errors.New("made-up error")}).NewClient
	require.EqualError(t, f.Initialize(metrics.NullFactory, zap.NewNop()), "failed to create Elasticsearch client: made-up error")

	f.newClientFn = (&mockClientBuilder{}).NewClient
	require.NoError(t, f.Initialize(metrics.NullFactory, zap.NewNop()))

	_, err := f.CreateSpanReader()
	require.NoError(t, err)

	_, err = f.CreateSpanWriter()
	require.NoError(t, err)

	_, err = f.CreateDependencyReader()
	require.NoError(t, err)

	_, err = f.CreateSamplingStore(1)
	require.NoError(t, err)

	require.NoError(t, f.Close())
}

func TestArchiveFactory(t *testing.T) {
	tests := []struct {
		name               string
		args               []string
		expectedReadAlias  string
		expectedWriteAlias string
	}{
		{
			name:               "default settings",
			args:               []string{},
			expectedReadAlias:  "archive",
			expectedWriteAlias: "archive",
		},
		{
			name:               "use read write aliases",
			args:               []string{"--es-archive.use-aliases=true"},
			expectedReadAlias:  "archive-read",
			expectedWriteAlias: "archive-write",
		},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			f := NewArchiveFactory()
			v, command := config.Viperize(f.AddFlags)
			command.ParseFlags(test.args)
			f.InitFromViper(v, zap.NewNop())

			f.newClientFn = (&mockClientBuilder{}).NewClient
			require.NoError(t, f.Initialize(metrics.NullFactory, zap.NewNop()))

			require.Equal(t, test.expectedReadAlias, f.config.ReadAliasSuffix)
			require.Equal(t, test.expectedWriteAlias, f.config.WriteAliasSuffix)
			require.True(t, f.config.UseReadWriteAliases)
		})
	}
}

func TestElasticsearchTagsFileDoNotExist(t *testing.T) {
	f := NewFactory()
	f.config = &escfg.Configuration{
		Tags: escfg.TagsAsFields{
			File: "fixtures/file-does-not-exist.txt",
		},
	}
	f.newClientFn = (&mockClientBuilder{}).NewClient
	require.NoError(t, f.Initialize(metrics.NullFactory, zap.NewNop()))
	defer f.Close()
	r, err := f.CreateSpanWriter()
	require.Error(t, err)
	assert.Nil(t, r)
}

func TestElasticsearchILMUsedWithoutReadWriteAliases(t *testing.T) {
	f := NewFactory()
	f.config = &escfg.Configuration{
		UseILM: true,
	}
	f.newClientFn = (&mockClientBuilder{}).NewClient
	require.NoError(t, f.Initialize(metrics.NullFactory, zap.NewNop()))
	defer f.Close()
	w, err := f.CreateSpanWriter()
	require.EqualError(t, err, "--es.use-ilm must always be used in conjunction with --es.use-aliases to ensure ES writers and readers refer to the single index mapping")
	assert.Nil(t, w)

	r, err := f.CreateSpanReader()
	require.EqualError(t, err, "--es.use-ilm must always be used in conjunction with --es.use-aliases to ensure ES writers and readers refer to the single index mapping")
	assert.Nil(t, r)
}

func TestTagKeysAsFields(t *testing.T) {
	tests := []struct {
		path          string
		include       string
		expected      []string
		errorExpected bool
	}{
		{
			path:          "fixtures/do_not_exists.txt",
			errorExpected: true,
		},
		{
			path:     "fixtures/tags_01.txt",
			expected: []string{"foo", "bar", "space"},
		},
		{
			path:     "fixtures/tags_02.txt",
			expected: nil,
		},
		{
			include:  "televators,eriatarka,thewidow",
			expected: []string{"televators", "eriatarka", "thewidow"},
		},
		{
			expected: nil,
		},
		{
			path:     "fixtures/tags_01.txt",
			include:  "televators,eriatarka,thewidow",
			expected: []string{"foo", "bar", "space", "televators", "eriatarka", "thewidow"},
		},
		{
			path:     "fixtures/tags_02.txt",
			include:  "televators,eriatarka,thewidow",
			expected: []string{"televators", "eriatarka", "thewidow"},
		},
	}

	for _, test := range tests {
		cfg := escfg.Configuration{
			Tags: escfg.TagsAsFields{
				File:    test.path,
				Include: test.include,
			},
		}

		tags, err := cfg.TagKeysAsFields()
		if test.errorExpected {
			require.Error(t, err)
			assert.Nil(t, tags)
		} else {
			require.NoError(t, err)
			assert.Equal(t, test.expected, tags)
		}
	}
}

func TestCreateTemplateError(t *testing.T) {
	f := NewFactory()
	f.config = &escfg.Configuration{CreateIndexTemplates: true}
	f.newClientFn = (&mockClientBuilder{createTemplateError: errors.New("template-error")}).NewClient
	err := f.Initialize(metrics.NullFactory, zap.NewNop())
	require.NoError(t, err)
	defer f.Close()

	w, err := f.CreateSpanWriter()
	assert.Nil(t, w)
	require.Error(t, err, "template-error")

	s, err := f.CreateSamplingStore(1)
	assert.Nil(t, s)
	require.Error(t, err, "template-error")
}

func TestILMDisableTemplateCreation(t *testing.T) {
	f := NewFactory()
	f.config = &escfg.Configuration{UseILM: true, UseReadWriteAliases: true, CreateIndexTemplates: true}
	f.newClientFn = (&mockClientBuilder{createTemplateError: errors.New("template-error")}).NewClient
	err := f.Initialize(metrics.NullFactory, zap.NewNop())
	defer f.Close()
	require.NoError(t, err)
	_, err = f.CreateSpanWriter()
	require.NoError(t, err) // as the createTemplate is not called, CreateSpanWriter should not return an error
}

func TestConfigureFromOptions(t *testing.T) {
	f := NewFactory()
	o := &Options{
		Config: namespaceConfig{Configuration: escfg.Configuration{Servers: []string{"server"}}},
	}
	f.configureFromOptions(o)
	assert.Equal(t, o.GetConfig(), f.config)
}

func TestESStorageFactoryWithConfig(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Write(mockEsServerResponse)
	}))
	defer server.Close()
	cfg := escfg.Configuration{
		Servers:  []string{server.URL},
		LogLevel: "error",
	}
	factory, err := NewFactoryWithConfig(cfg, metrics.NullFactory, zap.NewNop())
	require.NoError(t, err)
	defer factory.Close()
}

func TestConfigurationValidation(t *testing.T) {
	testCases := []struct {
		name    string
		cfg     escfg.Configuration
		wantErr bool
	}{
		{
			name: "valid configuration",
			cfg: escfg.Configuration{
				Servers: []string{"http://localhost:9200"},
			},
			wantErr: false,
		},
		{
			name:    "missing servers",
			cfg:     escfg.Configuration{},
			wantErr: true,
		},
	}
	for _, test := range testCases {
		t.Run(test.name, func(t *testing.T) {
			err := test.cfg.Validate()
			if test.wantErr {
				require.Error(t, err)
				_, err = NewFactoryWithConfig(test.cfg, metrics.NullFactory, zap.NewNop())
				require.Error(t, err)
			} else {
				require.NoError(t, err)
			}
		})
	}
}

func TestESStorageFactoryWithConfigError(t *testing.T) {
	defer testutils.VerifyGoLeaksOnce(t)

	cfg := escfg.Configuration{
		Servers:  []string{"http://127.0.0.1:65535"},
		LogLevel: "error",
	}
	_, err := NewFactoryWithConfig(cfg, metrics.NullFactory, zap.NewNop())
	require.ErrorContains(t, err, "failed to create Elasticsearch client")
}

func TestPasswordFromFile(t *testing.T) {
	defer testutils.VerifyGoLeaksOnce(t)
	t.Run("primary client", func(t *testing.T) {
		f := NewFactory()
		testPasswordFromFile(t, f, f.getClient, f.CreateSpanWriter)
	})

	t.Run("load token error", func(t *testing.T) {
		file := filepath.Join(t.TempDir(), "does not exist")
		token, err := loadTokenFromFile(file)
		require.Error(t, err)
		assert.Empty(t, token)
	})
}

func testPasswordFromFile(t *testing.T, f *Factory, getClient func() es.Client, getWriter func() (spanstore.Writer, error)) {
	const (
		pwd1 = "first password"
		pwd2 = "second password"
		// and with user name
		upwd1 = "user:" + pwd1
		upwd2 = "user:" + pwd2
	)
	var authReceived sync.Map
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		t.Logf("request to fake ES server: %v", r)
		// epecting header in the form Authorization:[Basic OmZpcnN0IHBhc3N3b3Jk]
		h := strings.Split(r.Header.Get("Authorization"), " ")
		if !assert.Len(t, h, 2) {
			return
		}
		assert.Equal(t, "Basic", h[0])
		authBytes, err := base64.StdEncoding.DecodeString(h[1])
		assert.NoError(t, err, "header: %s", h)
		auth := string(authBytes)
		authReceived.Store(auth, auth)
		t.Logf("request to fake ES server contained auth=%s", auth)
		w.Write(mockEsServerResponse)
	}))
	defer server.Close()

	pwdFile := filepath.Join(t.TempDir(), "pwd")
	require.NoError(t, os.WriteFile(pwdFile, []byte(pwd1), 0o600))

	f.config = &escfg.Configuration{
		Servers:  []string{server.URL},
		LogLevel: "debug",
		Authentication: escfg.Authentication{
			BasicAuthentication: escfg.BasicAuthentication{
				Username:         "user",
				PasswordFilePath: pwdFile,
			},
		},
		BulkProcessing: escfg.BulkProcessing{
			MaxBytes: -1, // disable bulk; we want immediate flush
		},
	}
	require.NoError(t, f.Initialize(metrics.NullFactory, zaptest.NewLogger(t)))
	defer f.Close()

	writer, err := getWriter()
	require.NoError(t, err)
	span := &model.Span{
		Process: &model.Process{ServiceName: "foo"},
	}
	require.NoError(t, writer.WriteSpan(context.Background(), span))
	assert.Eventually(t,
		func() bool {
			pwd, ok := authReceived.Load(upwd1)
			return ok && pwd == upwd1
		},
		5*time.Second, time.Millisecond,
		"expecting es.Client to send the first password",
	)

	t.Log("replace password in the file")
	client1 := getClient()
	newPwdFile := filepath.Join(t.TempDir(), "pwd2")
	require.NoError(t, os.WriteFile(newPwdFile, []byte(pwd2), 0o600))
	require.NoError(t, os.Rename(newPwdFile, pwdFile))

	assert.Eventually(t,
		func() bool {
			client2 := getClient()
			return client1 != client2
		},
		5*time.Second, time.Millisecond,
		"expecting es.Client to change for the new password",
	)

	require.NoError(t, writer.WriteSpan(context.Background(), span))
	assert.Eventually(t,
		func() bool {
			pwd, ok := authReceived.Load(upwd2)
			return ok && pwd == upwd2
		},
		5*time.Second, time.Millisecond,
		"expecting es.Client to send the new password",
	)
}

func TestFactoryESClientsAreNil(t *testing.T) {
	f := &Factory{}
	assert.Nil(t, f.getClient())
}

func TestPasswordFromFileErrors(t *testing.T) {
	defer testutils.VerifyGoLeaksOnce(t)
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Write(mockEsServerResponse)
	}))
	defer server.Close()

	pwdFile := filepath.Join(t.TempDir(), "pwd")
	require.NoError(t, os.WriteFile(pwdFile, []byte("first password"), 0o600))

	f := NewFactory()
	f.config = &escfg.Configuration{
		Servers:  []string{server.URL},
		LogLevel: "debug",
		Authentication: escfg.Authentication{
			BasicAuthentication: escfg.BasicAuthentication{
				PasswordFilePath: pwdFile,
			},
		},
	}

	logger, buf := testutils.NewEchoLogger(t)
	require.NoError(t, f.Initialize(metrics.NullFactory, logger))
	defer f.Close()

	f.config.Servers = []string{}
	f.onPasswordChange()
	assert.Contains(t, buf.String(), "no servers specified")

	require.NoError(t, os.Remove(pwdFile))
	f.onPasswordChange()
}

func TestInheritSettingsFrom(t *testing.T) {
	primaryFactory := NewFactory()
	primaryFactory.config = &escfg.Configuration{
		MaxDocCount: 99,
	}

	archiveFactory := NewArchiveFactory()
	archiveFactory.config = &escfg.Configuration{
		SendGetBodyAs: "PUT",
	}

	archiveFactory.InheritSettingsFrom(primaryFactory)

	require.Equal(t, "PUT", archiveFactory.config.SendGetBodyAs)
	require.Equal(t, 99, primaryFactory.config.MaxDocCount)
}

func TestIsArchiveCapable(t *testing.T) {
	tests := []struct {
		name      string
		namespace string
		enabled   bool
		expected  bool
	}{
		{
			name:      "archive capable",
			namespace: "es-archive",
			enabled:   true,
			expected:  true,
		},
		{
			name:      "not capable",
			namespace: "es-archive",
			enabled:   false,
			expected:  false,
		},
		{
			name:      "capable + wrong namespace",
			namespace: "es",
			enabled:   true,
			expected:  false,
		},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			factory := &Factory{
				Options: &Options{
					Config: namespaceConfig{
						namespace: test.namespace,
						Configuration: escfg.Configuration{
							Enabled: test.enabled,
						},
					},
				},
			}
			result := factory.IsArchiveCapable()
			require.Equal(t, test.expected, result)
		})
	}
}
